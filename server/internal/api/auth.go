package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/users"
)

// SessionCookie es la cookie de la sesión del panel. El prefijo __Host- obliga
// al navegador a aceptarla solo si es Secure, sin Domain y con Path=/: ningún
// subdominio puede pisarla.
const SessionCookie = "__Host-statussv_session"

// Límites del login. Un login real es de a uno; lo que pase de esto es alguien
// probando contraseñas. argon2id ya hace caro cada intento, y además hay un tope
// de hashes simultáneos en el servicio.
const (
	loginPerMinute = 5
	loginBurst     = 5
	// maxLoginBody alcanza para un usuario y una contraseña de 128 caracteres
	// cualesquiera, con margen.
	maxLoginBody = 4 << 10
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// sessionResponse describe la sesión. Stage dice en qué paso del login está:
// "full", o "totp_pending" / "totp_enroll" si falta el segundo factor.
type sessionResponse struct {
	Username  string    `json:"username"`
	ExpiresAt time.Time `json:"expires_at"`
	Stage     string    `json:"stage"`
	// RecoveryCodesLeft viaja solo con una sesión completa.
	RecoveryCodesLeft *int64 `json:"recovery_codes_left,omitempty"`
}

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// handleLogin abre una sesión y la entrega en una cookie HttpOnly: el código del
// panel nunca ve el token.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := s.clientIP(r)
	noStore(w)

	if !s.loginLimiter.allow(ip) {
		s.log.WarnContext(ctx, "login rechazado por límite de frecuencia", "ip", ip)
		writeJSON(ctx, w, http.StatusTooManyRequests, errorResponse{
			Error: "rate_limited", Message: "demasiados intentos; esperá un minuto",
		})
		return
	}

	var req loginRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLoginBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{
			Error: "invalid_request", Message: "se esperaba un JSON con username y password",
		})
		return
	}

	token, session, err := s.users.Login(ctx, req.Username, req.Password, ip, r.UserAgent())
	switch {
	case errors.Is(err, users.ErrInvalidCredentials):
		s.log.WarnContext(ctx, "login fallido", "usuario", loggableUsername(req.Username), "ip", ip)
		writeJSON(ctx, w, http.StatusUnauthorized, errorResponse{
			Error: "invalid_credentials", Message: "usuario o contraseña incorrectos",
		})
		return
	case err != nil:
		s.log.ErrorContext(ctx, "falló el login", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}

	s.log.InfoContext(ctx, "contraseña correcta, falta el segundo factor", "usuario", session.Username, "ip", ip, "etapa", session.Stage)
	http.SetCookie(w, sessionCookie(token.Reveal(), session.ExpiresAt))
	writeJSON(ctx, w, http.StatusOK, sessionResponse{Username: session.Username, ExpiresAt: session.ExpiresAt, Stage: session.Stage})
}

// handleLogout cierra la sesión y borra la cookie. Responde lo mismo haya o no
// sesión: salir dos veces no es un error.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)

	if cookie, err := r.Cookie(SessionCookie); err == nil {
		if err := s.users.Logout(ctx, auth.ParseSecret(cookie.Value)); err != nil {
			s.log.ErrorContext(ctx, "falló el logout", "error", err)
			writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
			return
		}
	}

	http.SetCookie(w, sessionCookie("", time.Unix(0, 0)))
	w.WriteHeader(http.StatusNoContent)
}

// handleMe dice quién es el usuario de la sesión y en qué paso del login está.
// Es lo que consulta el panel al abrir, para saber si mostrar el login, el
// código o el alta del segundo factor.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	session := SessionFrom(ctx)
	noStore(w)

	resp := sessionResponse{Username: session.Username, ExpiresAt: session.ExpiresAt, Stage: session.Stage}
	if session.Full() {
		left, err := s.users.RecoveryCodesLeft(ctx, session.UserID)
		if err != nil {
			s.log.ErrorContext(ctx, "no se pudieron contar los códigos de recuperación", "error", err)
			writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
			return
		}
		resp.RecoveryCodesLeft = &left
	}
	writeJSON(ctx, w, http.StatusOK, resp)
}

type sessionKey struct{}

// SessionFrom devuelve la sesión que dejó requireSession en el contexto.
func SessionFrom(ctx context.Context) users.Session {
	session, _ := ctx.Value(sessionKey{}).(users.Session)
	return session
}

// requireSession deja pasar solo los pedidos con una sesión completa, que pasó
// el segundo factor, y la pone en el contexto para el handler.
func (s *Server) requireSession(next http.Handler) http.Handler {
	return s.requireStage(next, users.StageFull)
}

// requireStage deja pasar solo los pedidos con una sesión viva en alguna de las
// etapas dadas.
func (s *Server) requireStage(next http.Handler, stages ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		cookie, err := r.Cookie(SessionCookie)
		if err != nil {
			unauthorized(ctx, w)
			return
		}
		session, err := s.users.Authenticate(ctx, auth.ParseSecret(cookie.Value))
		switch {
		case errors.Is(err, users.ErrNoSession):
			// La cookie no sirve más: se borra para que el navegador deje de
			// mandarla.
			http.SetCookie(w, sessionCookie("", time.Unix(0, 0)))
			unauthorized(ctx, w)
			return
		case err != nil:
			s.log.ErrorContext(ctx, "falló la verificación de la sesión", "error", err)
			writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
			return
		}
		if !slices.Contains(stages, session.Stage) {
			noStore(w)
			if slices.Contains(stages, users.StageFull) {
				// El panel entiende este error como "mostrá la pantalla del código".
				writeJSON(ctx, w, http.StatusUnauthorized, errorResponse{
					Error: "second_factor_required", Message: "falta el segundo factor",
				})
				return
			}
			writeJSON(ctx, w, http.StatusConflict, errorResponse{
				Error: "wrong_stage", Message: "este paso no corresponde a la sesión",
			})
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, sessionKey{}, session)))
	})
}

func unauthorized(ctx context.Context, w http.ResponseWriter) {
	noStore(w)
	writeJSON(ctx, w, http.StatusUnauthorized, errorResponse{Error: "unauthorized", Message: "iniciá sesión"})
}

// sessionCookie arma la cookie de sesión. HttpOnly la esconde del JavaScript
// del panel, Secure la manda solo por HTTPS y SameSite=Strict no la manda en
// pedidos que arrancan en otro sitio, que es la primera defensa contra CSRF.
// Con un valor vacío y un vencimiento en el pasado, la borra.
func sessionCookie(value string, expires time.Time) *http.Cookie {
	cookie := &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}
	if value == "" {
		cookie.MaxAge = -1
	}
	return cookie
}

// noStore evita que un proxy o el navegador guarden respuestas con datos de la
// sesión.
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

// loggableUsername devuelve el nombre para el registro solo si tiene formato de
// nombre de usuario. Si alguien escribe la contraseña en el campo equivocado, no
// tiene que terminar en un log.
func loggableUsername(username string) string {
	normalized := users.NormalizeUsername(username)
	if !users.ValidUsername(normalized) {
		return "(inválido)"
	}
	return normalized
}
