package users

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// TOTPIssuer es el nombre con el que aparece la cuenta en la app de
// autenticación.
const TOTPIssuer = "status-sv"

// MaxCodeAttempts es cuántos códigos equivocados admite una sesión a medias.
// Con el quinto se cierra, y hay que volver a poner la contraseña.
const MaxCodeAttempts = 5

var (
	// ErrInvalidCode es un código TOTP o de recuperación que no sirve.
	ErrInvalidCode = errors.New("código incorrecto")
	// ErrTooManyAttempts es una sesión a medias que se cerró por códigos
	// equivocados.
	ErrTooManyAttempts = errors.New("demasiados códigos incorrectos; volvé a iniciar sesión")
	// ErrWrongStage es un paso del login que no corresponde a la etapa de la
	// sesión: por ejemplo, confirmar un alta cuando el usuario ya tiene TOTP.
	ErrWrongStage = errors.New("este paso no corresponde a la sesión")
	// ErrNoPendingEnrollment es una confirmación sin un alta empezada.
	ErrNoPendingEnrollment = errors.New("no hay un alta de TOTP en curso")
)

// Enrollment es un alta de TOTP en curso: lo que la persona carga en su app.
type Enrollment struct {
	// Secret es el secreto en base32, para cargarlo a mano.
	Secret string
	// URI es el enlace otpauth:// que va en el QR.
	URI string
}

// BeginEnrollment empieza el alta de TOTP de una sesión a medias que todavía no
// lo tiene. Empezarla de nuevo descarta el secreto anterior sin confirmar.
func (s *Service) BeginEnrollment(ctx context.Context, session Session) (Enrollment, error) {
	if session.Stage != StageTOTPEnroll {
		return Enrollment{}, ErrWrongStage
	}
	secret, err := auth.NewTOTPSecret()
	if err != nil {
		return Enrollment{}, err
	}
	sealed, err := s.totpKey.Seal(secret, session.UserID[:])
	if err != nil {
		return Enrollment{}, err
	}
	n, err := s.q.SetPendingTOTP(ctx, store.SetPendingTOTPParams{ID: session.UserID, Pending: sealed})
	if err != nil {
		return Enrollment{}, fmt.Errorf("guardar el alta: %w", err)
	}
	if n == 0 {
		// El usuario ya tiene TOTP: otra sesión terminó el alta en el medio.
		return Enrollment{}, ErrWrongStage
	}
	return Enrollment{
		Secret: auth.EncodeTOTPSecret(secret),
		URI:    auth.TOTPURI(TOTPIssuer, session.Username, secret),
	}, nil
}

// ConfirmEnrollment termina el alta con un código de la app. Cambia la sesión a
// medias por una completa, con otro token, y devuelve los códigos de
// recuperación, que no vuelven a estar disponibles.
func (s *Service) ConfirmEnrollment(ctx context.Context, session Session, code, ip, userAgent string) (auth.Secret, Session, []string, error) {
	if session.Stage != StageTOTPEnroll {
		return auth.Secret{}, Session{}, nil, ErrWrongStage
	}
	user, err := s.user(ctx, session.UserID)
	if err != nil {
		return auth.Secret{}, Session{}, nil, err
	}
	if user.TotpPendingSecret == nil {
		return auth.Secret{}, Session{}, nil, ErrNoPendingEnrollment
	}
	secret, err := s.totpKey.Open(user.TotpPendingSecret, user.ID[:])
	if err != nil {
		return auth.Secret{}, Session{}, nil, err
	}
	step, ok := auth.VerifyTOTP(secret, code, s.now(), 0)
	if !ok {
		return auth.Secret{}, Session{}, nil, s.failedAttempt(ctx, session)
	}

	codes, err := auth.NewRecoveryCodes()
	if err != nil {
		return auth.Secret{}, Session{}, nil, err
	}

	var (
		token auth.Secret
		full  Session
	)
	err = s.inTx(ctx, func(q *store.Queries) error {
		n, err := q.EnableTOTP(ctx, store.EnableTOTPParams{
			ID: user.ID, Pending: user.TotpPendingSecret, Step: step, Now: s.now().UTC(),
		})
		if err != nil {
			return fmt.Errorf("activar el TOTP: %w", err)
		}
		if n == 0 {
			// Otra pestaña empezó otra alta o ya la terminó.
			return ErrNoPendingEnrollment
		}
		if err := s.replaceRecoveryCodes(ctx, q, user.ID, codes); err != nil {
			return err
		}
		token, full, err = s.promote(ctx, q, session, ip, userAgent)
		return err
	})
	if err != nil {
		return auth.Secret{}, Session{}, nil, err
	}
	return token, full, codes, nil
}

// VerifyTOTP pasa una sesión a medias a completa con un código de la app.
func (s *Service) VerifyTOTP(ctx context.Context, session Session, code, ip, userAgent string) (auth.Secret, Session, error) {
	if session.Stage != StageTOTPPending {
		return auth.Secret{}, Session{}, ErrWrongStage
	}
	user, err := s.user(ctx, session.UserID)
	if err != nil {
		return auth.Secret{}, Session{}, err
	}
	if user.TotpSecret == nil {
		// Alguien reseteó el TOTP mientras tanto.
		return auth.Secret{}, Session{}, ErrWrongStage
	}
	secret, err := s.totpKey.Open(user.TotpSecret, user.ID[:])
	if err != nil {
		return auth.Secret{}, Session{}, err
	}
	step, ok := auth.VerifyTOTP(secret, code, s.now(), user.TotpLastStep)
	if !ok {
		return auth.Secret{}, Session{}, s.failedAttempt(ctx, session)
	}

	var (
		token auth.Secret
		full  Session
	)
	err = s.inTx(ctx, func(q *store.Queries) error {
		n, err := q.AdvanceTOTPStep(ctx, store.AdvanceTOTPStepParams{ID: user.ID, Step: step})
		if err != nil {
			return fmt.Errorf("anotar el código usado: %w", err)
		}
		if n == 0 {
			// Otro pedido usó el mismo código un instante antes.
			return ErrInvalidCode
		}
		token, full, err = s.promote(ctx, q, session, ip, userAgent)
		return err
	})
	if errors.Is(err, ErrInvalidCode) {
		return auth.Secret{}, Session{}, s.failedAttempt(ctx, session)
	}
	if err != nil {
		return auth.Secret{}, Session{}, err
	}
	return token, full, nil
}

// ConfirmAction verifica un código TOTP nuevo de una sesión completa, para una
// acción que lo pide además de la sesión: desplegar en producción (ADR-017).
// El código queda usado y no sirve dos veces. No acepta códigos de
// recuperación: esos son para entrar, no para confirmar.
//
// Un código equivocado no cierra la sesión; lo que frena probar códigos es el
// límite por IP del handler.
func (s *Service) ConfirmAction(ctx context.Context, session Session, code string) error {
	if !session.Full() {
		return ErrWrongStage
	}
	user, err := s.user(ctx, session.UserID)
	if err != nil {
		return err
	}
	if user.TotpSecret == nil {
		return ErrWrongStage
	}
	secret, err := s.totpKey.Open(user.TotpSecret, user.ID[:])
	if err != nil {
		return err
	}
	step, ok := auth.VerifyTOTP(secret, code, s.now(), user.TotpLastStep)
	if !ok {
		return ErrInvalidCode
	}
	n, err := s.q.AdvanceTOTPStep(ctx, store.AdvanceTOTPStepParams{ID: user.ID, Step: step})
	if err != nil {
		return fmt.Errorf("anotar el código usado: %w", err)
	}
	if n == 0 {
		// Otro pedido usó el mismo código un instante antes.
		return ErrInvalidCode
	}
	return nil
}

// VerifyRecoveryCode pasa una sesión a medias a completa con un código de
// recuperación, que queda usado.
func (s *Service) VerifyRecoveryCode(ctx context.Context, session Session, code, ip, userAgent string) (auth.Secret, Session, error) {
	if session.Stage != StageTOTPPending {
		return auth.Secret{}, Session{}, ErrWrongStage
	}
	normalized, ok := auth.NormalizeRecoveryCode(code)
	if !ok {
		return auth.Secret{}, Session{}, s.failedAttempt(ctx, session)
	}

	var (
		token auth.Secret
		full  Session
	)
	err := s.inTx(ctx, func(q *store.Queries) error {
		n, err := q.UseRecoveryCode(ctx, store.UseRecoveryCodeParams{
			UserID: session.UserID, CodeHash: auth.HashRecoveryCode(normalized), Now: s.now().UTC(),
		})
		if err != nil {
			return fmt.Errorf("usar el código de recuperación: %w", err)
		}
		if n == 0 {
			return ErrInvalidCode
		}
		token, full, err = s.promote(ctx, q, session, ip, userAgent)
		return err
	})
	if errors.Is(err, ErrInvalidCode) {
		return auth.Secret{}, Session{}, s.failedAttempt(ctx, session)
	}
	if err != nil {
		return auth.Secret{}, Session{}, err
	}
	s.log.WarnContext(ctx, "login con un código de recuperación", "usuario", session.Username)
	return token, full, nil
}

// RecoveryCodesLeft cuenta los códigos de recuperación sin usar de un usuario.
func (s *Service) RecoveryCodesLeft(ctx context.Context, userID uuid.UUID) (int64, error) {
	n, err := s.q.CountUnusedRecoveryCodes(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("contar los códigos de recuperación: %w", err)
	}
	return n, nil
}

// ResetTOTP borra el segundo factor y los códigos de recuperación de un
// usuario, y cierra todas sus sesiones. Es para quien perdió el teléfono y los
// códigos; lo corre una persona con SSH (ADR-013).
func (s *Service) ResetTOTP(ctx context.Context, username string) (closedSessions int64, err error) {
	user, err := s.q.GetUserByUsername(ctx, NormalizeUsername(username))
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrUserNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("buscar el usuario: %w", err)
	}

	err = s.inTx(ctx, func(q *store.Queries) error {
		if _, err := q.ResetTOTP(ctx, user.ID); err != nil {
			return fmt.Errorf("borrar el TOTP: %w", err)
		}
		if _, err := q.DeleteRecoveryCodes(ctx, user.ID); err != nil {
			return fmt.Errorf("borrar los códigos de recuperación: %w", err)
		}
		closedSessions, err = q.DeleteUserSessions(ctx, user.ID)
		if err != nil {
			return fmt.Errorf("cerrar las sesiones: %w", err)
		}
		return nil
	})
	return closedSessions, err
}

// promote cierra la sesión a medias y abre una completa con otro token: el de
// la etapa anterior no sirve para nada más.
func (s *Service) promote(ctx context.Context, q *store.Queries, session Session, ip, userAgent string) (auth.Secret, Session, error) {
	n, err := q.DeleteSession(ctx, session.ID)
	if err != nil {
		return auth.Secret{}, Session{}, fmt.Errorf("cerrar la sesión a medias: %w", err)
	}
	if n == 0 {
		// Otro pedido con el mismo token ya la cambió.
		return auth.Secret{}, Session{}, ErrNoSession
	}
	return s.openSession(ctx, q, session.UserID, session.Username, StageFull, ip, userAgent)
}

// failedAttempt anota un código equivocado. Con el último intento cierra la
// sesión a medias.
func (s *Service) failedAttempt(ctx context.Context, session Session) error {
	attempts, err := s.q.CountSessionAttempt(ctx, session.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoSession
	}
	if err != nil {
		return fmt.Errorf("anotar el intento: %w", err)
	}
	if attempts < MaxCodeAttempts {
		return ErrInvalidCode
	}
	if _, err := s.q.DeleteSession(ctx, session.ID); err != nil {
		return fmt.Errorf("cerrar la sesión: %w", err)
	}
	s.log.WarnContext(ctx, "sesión cerrada por códigos incorrectos", "usuario", session.Username)
	return ErrTooManyAttempts
}

func (s *Service) replaceRecoveryCodes(ctx context.Context, q *store.Queries, userID uuid.UUID, codes []string) error {
	if _, err := q.DeleteRecoveryCodes(ctx, userID); err != nil {
		return fmt.Errorf("borrar los códigos de recuperación: %w", err)
	}
	ids := make([]uuid.UUID, len(codes))
	hashes := make([][]byte, len(codes))
	for i, code := range codes {
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("generar el ID: %w", err)
		}
		normalized, ok := auth.NormalizeRecoveryCode(code)
		if !ok {
			return fmt.Errorf("código de recuperación mal generado")
		}
		ids[i], hashes[i] = id, auth.HashRecoveryCode(normalized)
	}
	if _, err := q.InsertRecoveryCodes(ctx, store.InsertRecoveryCodesParams{UserID: userID, Ids: ids, Hashes: hashes}); err != nil {
		return fmt.Errorf("guardar los códigos de recuperación: %w", err)
	}
	return nil
}

func (s *Service) user(ctx context.Context, id uuid.UUID) (store.User, error) {
	user, err := s.q.GetUserByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.User{}, ErrNoSession
	}
	if err != nil {
		return store.User{}, fmt.Errorf("buscar el usuario: %w", err)
	}
	return user, nil
}

func (s *Service) inTx(ctx context.Context, fn func(q *store.Queries) error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("abrir la transacción: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(store.New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("confirmar la transacción: %w", err)
	}
	return nil
}
