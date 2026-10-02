package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"rsc.io/qr"

	"github.com/JuanBaez77/status-sv/server/internal/audit"
	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/users"
)

// Límites del segundo factor por IP. Cada sesión a medias admite además
// users.MaxCodeAttempts códigos equivocados: esto frena a quien abre sesiones
// a medias en serie desde la misma IP.
const (
	totpPerMinute = 10
	totpBurst     = 10
	maxTOTPBody   = 1 << 10
)

type totpEnrollResponse struct {
	// Secret es el secreto en base32, para cargarlo a mano en la app.
	Secret string `json:"secret"`
	// URI es el enlace otpauth:// que lleva el QR.
	URI string `json:"otpauth_uri"`
	// QR es la imagen PNG del enlace, como data URL, lista para un <img>.
	QR string `json:"qr_png"`
}

type totpCodeRequest struct {
	Code         string `json:"code"`
	RecoveryCode string `json:"recovery_code"`
}

type totpConfirmResponse struct {
	sessionResponse
	// RecoveryCodes se muestran esta única vez.
	RecoveryCodes []string `json:"recovery_codes"`
}

// handleTOTPEnroll empieza el alta del segundo factor: genera el secreto y
// devuelve el QR. Solo con la sesión a medias de alguien que todavía no tiene
// TOTP.
func (s *Server) handleTOTPEnroll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)
	if !s.totpLimiter.allow(s.clientIP(r)) {
		s.blockedTOTP(w, r, audit.ActionTOTPEnroll)
		return
	}

	enrollment, err := s.users.BeginEnrollment(ctx, SessionFrom(ctx))
	if err != nil {
		s.totpError(w, r, audit.ActionTOTPEnroll, false, err)
		return
	}
	code, err := qr.Encode(enrollment.URI, qr.M)
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo armar el QR", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	writeJSON(ctx, w, http.StatusOK, totpEnrollResponse{
		Secret: enrollment.Secret,
		URI:    enrollment.URI,
		QR:     "data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG()),
	})
}

// handleTOTPConfirm termina el alta con un código de la app. La sesión a medias
// se cambia por una completa, en una cookie nueva, y se entregan los códigos de
// recuperación.
func (s *Server) handleTOTPConfirm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := s.clientIP(r)
	noStore(w)
	if !s.totpLimiter.allow(ip) {
		s.blockedTOTP(w, r, audit.ActionTOTPEnroll)
		return
	}
	req, ok := decodeTOTPRequest(w, r)
	if !ok {
		return
	}

	session := SessionFrom(ctx)
	token, full, codes, err := s.users.ConfirmEnrollment(ctx, session, req.Code, ip, r.UserAgent())
	if err != nil {
		s.totpError(w, r, audit.ActionTOTPEnroll, false, err)
		return
	}
	s.recordAccess(ctx, audit.ActionTOTPEnroll, full.Username, audit.ResultOK, ip, nil)
	s.log.InfoContext(ctx, "segundo factor activado; login", "usuario", full.Username, "ip", ip)
	http.SetCookie(w, sessionCookie(token.Reveal(), full.ExpiresAt))
	left := int64(len(codes))
	writeJSON(ctx, w, http.StatusOK, totpConfirmResponse{
		sessionResponse: sessionResponse{
			Username: full.Username, ExpiresAt: full.ExpiresAt, Stage: full.Stage, RecoveryCodesLeft: &left,
		},
		RecoveryCodes: codes,
	})
}

// handleTOTPVerify completa el login con un código de la app o uno de
// recuperación.
func (s *Server) handleTOTPVerify(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := s.clientIP(r)
	noStore(w)
	if !s.totpLimiter.allow(ip) {
		s.blockedTOTP(w, r, audit.ActionTOTPVerify)
		return
	}
	req, ok := decodeTOTPRequest(w, r)
	if !ok {
		return
	}

	session := SessionFrom(ctx)
	var (
		token auth.Secret
		full  users.Session
		err   error
	)
	if req.RecoveryCode != "" {
		token, full, err = s.users.VerifyRecoveryCode(ctx, session, req.RecoveryCode, ip, r.UserAgent())
	} else {
		token, full, err = s.users.VerifyTOTP(ctx, session, req.Code, ip, r.UserAgent())
	}
	if err != nil {
		s.totpError(w, r, audit.ActionTOTPVerify, req.RecoveryCode != "", err)
		return
	}

	// Entrar con un código de recuperación es otra acción: gasta un código y
	// conviene verlo aparte.
	action := audit.ActionTOTPVerify
	if req.RecoveryCode != "" {
		action = audit.ActionRecoveryCode
	}
	s.recordAccess(ctx, action, full.Username, audit.ResultOK, ip, nil)
	s.log.InfoContext(ctx, "login", "usuario", full.Username, "ip", ip, "con_codigo_de_recuperacion", req.RecoveryCode != "")
	http.SetCookie(w, sessionCookie(token.Reveal(), full.ExpiresAt))
	left, err := s.users.RecoveryCodesLeft(ctx, full.UserID)
	if err != nil {
		// El login ya se hizo: sin el conteo, el panel no muestra el aviso.
		s.log.WarnContext(ctx, "no se pudieron contar los códigos de recuperación", "error", err)
		writeJSON(ctx, w, http.StatusOK, sessionResponse{Username: full.Username, ExpiresAt: full.ExpiresAt, Stage: full.Stage})
		return
	}
	writeJSON(ctx, w, http.StatusOK, sessionResponse{
		Username: full.Username, ExpiresAt: full.ExpiresAt, Stage: full.Stage, RecoveryCodesLeft: &left,
	})
}

// decodeTOTPRequest lee el pedido con un código: exactamente uno de los dos.
func decodeTOTPRequest(w http.ResponseWriter, r *http.Request) (totpCodeRequest, bool) {
	var req totpCodeRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTOTPBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || (req.Code == "") == (req.RecoveryCode == "") {
		writeJSON(r.Context(), w, http.StatusBadRequest, errorResponse{
			Error: "invalid_request", Message: "se esperaba un JSON con code o con recovery_code",
		})
		return totpCodeRequest{}, false
	}
	return req, true
}

// totpError contesta un error del segundo factor y deja en la auditoría los
// códigos equivocados. Un código equivocado no dice cuántos intentos quedan:
// no le sirve a nadie más que a quien está probando.
func (s *Server) totpError(w http.ResponseWriter, r *http.Request, action string, recovery bool, err error) {
	ctx := r.Context()
	session := SessionFrom(ctx)
	if errors.Is(err, users.ErrInvalidCode) || errors.Is(err, users.ErrTooManyAttempts) {
		detail := map[string]any{}
		if recovery {
			detail["con_codigo_de_recuperacion"] = true
		}
		if errors.Is(err, users.ErrTooManyAttempts) {
			detail["sesion_cerrada"] = true
		}
		s.recordAccess(ctx, action, session.Username, audit.ResultFailed, s.clientIP(r), detail)
	}
	switch {
	case errors.Is(err, users.ErrInvalidCode):
		s.log.WarnContext(ctx, "código de segundo factor incorrecto", "usuario", session.Username, "ip", s.clientIP(r))
		writeJSON(ctx, w, http.StatusUnauthorized, errorResponse{Error: "invalid_code", Message: "código incorrecto"})
	case errors.Is(err, users.ErrTooManyAttempts), errors.Is(err, users.ErrNoSession):
		// La sesión a medias se cerró: se borra la cookie y hay que volver a la
		// contraseña.
		http.SetCookie(w, sessionCookie("", time.Unix(0, 0)))
		writeJSON(ctx, w, http.StatusUnauthorized, errorResponse{Error: "unauthorized", Message: err.Error()})
	case errors.Is(err, users.ErrWrongStage), errors.Is(err, users.ErrNoPendingEnrollment):
		writeJSON(ctx, w, http.StatusConflict, errorResponse{Error: "wrong_stage", Message: err.Error()})
	default:
		s.log.ErrorContext(ctx, "falló el segundo factor", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
	}
}

// blockedTOTP contesta un pedido del segundo factor frenado por el límite de
// intentos y lo deja en la auditoría.
func (s *Server) blockedTOTP(w http.ResponseWriter, r *http.Request, action string) {
	username := SessionFrom(r.Context()).Username
	s.recordBlocked(r.Context(), audit.Entry{
		Actor: username, ActorType: audit.ActorUser, Action: action,
		TargetType: "user", TargetID: username, IP: s.clientIP(r),
	})
	tooManyTOTP(w, r)
}

func tooManyTOTP(w http.ResponseWriter, r *http.Request) {
	writeJSON(r.Context(), w, http.StatusTooManyRequests, errorResponse{
		Error: "rate_limited", Message: "demasiados intentos; esperá un minuto",
	})
}
