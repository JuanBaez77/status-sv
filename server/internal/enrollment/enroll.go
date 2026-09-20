package enrollment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// Errores que puede devolver Enroll. El handler los traduce a un código del
// protocolo; ninguno de estos mensajes le dice al agent por qué falló con más
// detalle del necesario, para no ayudar a quien esté probando tokens.
var (
	// ErrTokenNotFound cubre los tres casos a propósito: no existe, ya se usó o
	// venció. Distinguirlos le daría información a quien prueba tokens al azar.
	ErrTokenNotFound = errors.New("el token no existe, ya se usó o venció")
	// ErrWrongSecretKind es una credencial de agent usada donde va un token.
	ErrWrongSecretKind = errors.New("eso no es un token de enrolamiento")
	// ErrHostnameRequired falta el hostname del servidor que se enrola.
	ErrHostnameRequired = errors.New("falta el hostname")
)

// Result es lo que deja un enrolamiento exitoso.
type Result struct {
	// Server es el registro del servidor, nuevo o reutilizado.
	Server store.Server
	// Credential es la credencial propia del agent, en claro. Es la única vez
	// que existe: la base guarda solo su hash.
	Credential auth.Secret
	// Rotated dice si se revocó una credencial anterior, o sea si este servidor
	// ya estaba enrolado y se volvió a enrolar.
	Rotated bool
}

// Enroll canjea un token de un solo uso por la credencial propia del agent.
//
// Todo pasa en una transacción: si el token resulta consumido por otro pedido a
// mitad de camino, el servidor que se hubiera creado se deshace con el rollback.
// La condición de un solo uso la garantiza la base, no este código: la sentencia
// que consume el token exige que siga sin usar y sin vencer.
//
// Si el servidor ya existía (una reinstalación del VPS, por ejemplo), se
// reutiliza el registro, se actualiza su hostname y se revoca la credencial
// anterior en la misma transacción: así no quedan servidores duplicados ni dos
// credenciales válidas para la misma máquina.
func (s *Service) Enroll(ctx context.Context, token auth.Secret, hostname, agentVersion string) (Result, error) {
	if token.IsZero() {
		return Result{}, ErrTokenNotFound
	}
	if !token.HasPrefix(auth.EnrollmentTokenPrefix) {
		return Result{}, ErrWrongSecretKind
	}
	if hostname == "" {
		return Result{}, ErrHostnameRequired
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("abrir la transacción: %w", err)
	}
	// Si algo sale mal antes del commit, esto deshace todo. Después del commit
	// no tiene efecto.
	defer func() { _ = tx.Rollback(ctx) }()

	queries := s.queries.WithTx(tx)
	tokenHash := token.Hash()

	// Se lee el token bloqueando su fila: si otro pedido está enrolando con el
	// mismo token, este espera acá y después lo encuentra consumido, en vez de
	// avanzar en paralelo y chocar más adelante.
	pending, err := queries.GetEnrollmentTokenByHashForUpdate(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Result{}, ErrTokenNotFound
		}
		return Result{}, fmt.Errorf("buscar el token: %w", err)
	}
	if pending.UsedAt != nil || !pending.ExpiresAt.After(time.Now()) {
		return Result{}, ErrTokenNotFound
	}

	// Y se serializa por nombre de servidor: dos tokens distintos para el mismo
	// nombre no pueden intentar crearlo a la vez.
	if err := queries.LockServerName(ctx, pending.ServerName); err != nil {
		return Result{}, fmt.Errorf("tomar el lock del nombre de servidor: %w", err)
	}

	server, rotated, err := s.upsertServer(ctx, queries, pending.ServerName, hostname, agentVersion)
	if err != nil {
		return Result{}, err
	}

	// Acá se decide todo: si el token ya no está disponible, no vuelve ninguna
	// fila y el enrolamiento entero se deshace.
	if _, err := queries.ConsumeEnrollmentToken(ctx, store.ConsumeEnrollmentTokenParams{
		TokenHash: tokenHash,
		ServerID:  &server.ID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Result{}, ErrTokenNotFound
		}
		return Result{}, fmt.Errorf("consumir el token: %w", err)
	}

	credential, err := auth.NewAgentCredential()
	if err != nil {
		return Result{}, fmt.Errorf("generar la credencial: %w", err)
	}

	credentialID, err := uuid.NewV7()
	if err != nil {
		return Result{}, fmt.Errorf("generar el ID de la credencial: %w", err)
	}

	if _, err := queries.CreateAgentCredential(ctx, store.CreateAgentCredentialParams{
		ID:         credentialID,
		ServerID:   server.ID,
		SecretHash: credential.Hash(),
	}); err != nil {
		return Result{}, fmt.Errorf("guardar la credencial: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("confirmar el enrolamiento: %w", err)
	}

	s.log.InfoContext(ctx, "agent enrolado",
		"servidor", server.Name,
		"servidor_id", server.ID,
		"hostname", hostname,
		"version_agent", agentVersion,
		"credencial_rotada", rotated,
	)

	return Result{Server: server, Credential: credential, Rotated: rotated}, nil
}

// upsertServer devuelve el servidor con ese nombre, creándolo si no existía. Si
// ya existía, actualiza su hostname y revoca la credencial que estuviera activa.
func (s *Service) upsertServer(
	ctx context.Context,
	queries *store.Queries,
	serverName, hostname, agentVersion string,
) (store.Server, bool, error) {
	existing, err := queries.GetServerByName(ctx, serverName)
	switch {
	case err == nil:
		revoked, err := queries.RevokeActiveCredentialsForServer(ctx, store.RevokeActiveCredentialsForServerParams{
			ServerID: existing.ID,
			Reason:   ptr("reemplazada por un nuevo enrolamiento"),
		})
		if err != nil {
			return store.Server{}, false, fmt.Errorf("revocar las credenciales anteriores: %w", err)
		}

		updated, err := queries.UpdateServerHostname(ctx, store.UpdateServerHostnameParams{
			ID:       existing.ID,
			Hostname: hostname,
		})
		if err != nil {
			return store.Server{}, false, fmt.Errorf("actualizar el hostname: %w", err)
		}
		return updated, revoked > 0, nil

	case errors.Is(err, pgx.ErrNoRows):
		id, err := uuid.NewV7()
		if err != nil {
			return store.Server{}, false, fmt.Errorf("generar el ID del servidor: %w", err)
		}
		created, err := queries.CreateServer(ctx, store.CreateServerParams{
			ID:           id,
			Name:         serverName,
			Hostname:     hostname,
			Labels:       []byte(`{}`),
			AgentVersion: agentVersion,
		})
		if err != nil {
			return store.Server{}, false, fmt.Errorf("crear el servidor: %w", err)
		}
		return created, false, nil

	default:
		return store.Server{}, false, fmt.Errorf("buscar el servidor %q: %w", serverName, err)
	}
}

// Authenticate busca el servidor dueño de una credencial activa. Es lo que se
// ejecuta al abrir el WebSocket, una vez por conexión.
func (s *Service) Authenticate(ctx context.Context, credential auth.Secret) (store.Server, error) {
	if credential.IsZero() || !credential.HasPrefix(auth.AgentCredentialPrefix) {
		return store.Server{}, ErrUnauthorized
	}

	found, err := s.queries.GetActiveCredentialByHash(ctx, credential.Hash())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Server{}, ErrUnauthorized
		}
		return store.Server{}, fmt.Errorf("buscar la credencial: %w", err)
	}

	server, err := s.queries.GetServerByID(ctx, found.ServerID)
	if err != nil {
		return store.Server{}, fmt.Errorf("buscar el servidor de la credencial: %w", err)
	}

	// Deja constancia de que la credencial se usó. No es crítico: si falla, la
	// conexión sigue igual, solo se pierde el dato de último uso.
	if err := s.queries.TouchCredentialLastUsed(ctx, found.ID); err != nil {
		s.log.WarnContext(ctx, "no se pudo registrar el último uso de la credencial",
			"credencial_id", found.ID, "error", err)
	}

	return server, nil
}

// ErrUnauthorized es una credencial que no existe o fue revocada.
var ErrUnauthorized = errors.New("credencial inválida o revocada")

func ptr[T any](v T) *T { return &v }
