package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/spf13/cobra"
)

func newSessionsCommand(runtime *commandRuntime) *cobra.Command {
	command := &cobra.Command{Use: "sessions", Short: "Create and manage PM-only login sessions", Args: noArgs}
	command.AddCommand(newDeviceStartCommand(runtime))
	command.AddCommand(newDeviceExchangeCommand(runtime))
	command.AddCommand(&cobra.Command{
		Use:   "inspect",
		Short: "Inspect the current authenticated PM session",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, false, true)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.API().GetCurrentSessionWithResponse(ctx, nil)
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	})
	command.AddCommand(&cobra.Command{
		Use:   "logout",
		Short: "Revoke the current PM session and remove its OS-store record",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, false, true)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.API().RevokeCurrentSessionWithResponse(ctx, nil)
			if err != nil {
				return requestError(ctx, err)
			}
			envelope, err := pmapi.DecodeEnvelope(response)
			if err != nil {
				return err
			}
			if err := pmapi.EnvelopeError(envelope); err != nil {
				return err
			}
			key, err := pmapi.NewSessionKey(bundle.config.profile.InstanceID, bundle.config.profile.SessionHandle)
			if err != nil {
				return err
			}
			if err := bundle.store.Delete(ctx, key); err != nil {
				return err
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), "logged out")
			return err
		},
	})
	return command
}

func newDeviceStartCommand(runtime *commandRuntime) *cobra.Command {
	var deviceID singleString
	deviceID.name = "device-id"
	command := &cobra.Command{
		Use:   "login-start",
		Short: "Start a bounded device login challenge",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if !deviceID.set {
				return usageFailure(errors.New("--device-id is required"))
			}
			ctx, cancel, bundle, err := runtime.requestBundle(command, false, false)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.API().StartDeviceLoginWithResponse(ctx, pmapi.DeviceStart{DeviceId: deviceID.value})
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	}
	command.Flags().Var(&deviceID, deviceID.name, "Non-secret device identity")
	return command
}

func newDeviceExchangeCommand(runtime *commandRuntime) *cobra.Command {
	var challengeID singleString
	challengeID.name = "challenge-id"
	command := &cobra.Command{
		Use:   "login-exchange",
		Short: "Read a device proof from stdin and store the resulting PM session",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if !challengeID.set {
				return usageFailure(errors.New("--challenge-id is required"))
			}
			proof, err := readBoundedSecret(command.InOrStdin())
			if err != nil {
				return usageFailure(err)
			}
			ctx, cancel, bundle, err := runtime.requestBundle(command, false, false)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.API().ExchangeDeviceLoginWithResponse(ctx, pmapi.DeviceProof{ChallengeId: challengeID.value, Proof: proof})
			if err != nil {
				return requestError(ctx, err)
			}
			envelope, err := pmapi.DecodeEnvelope(response)
			if err != nil {
				return err
			}
			if err := pmapi.EnvelopeError(envelope); err != nil {
				return err
			}
			if response.JSON200 == nil {
				return pmapi.ErrMalformedEnvelope
			}
			factory := runtime.options.SessionStore
			if factory == nil {
				factory = pmapi.NewSessionStore
			}
			store, err := factory(bundle.config.sessionMode)
			if err != nil {
				return pmapi.ErrSessionStoreUnavailable
			}
			key, err := pmapi.NewSessionKey(bundle.config.profile.InstanceID, bundle.config.profile.SessionHandle)
			if err != nil {
				return err
			}
			if err := store.Store(ctx, key, *response.JSON200); err != nil {
				return err
			}
			safe := struct {
				SessionID string `json:"session_id"`
				ExpiresAt string `json:"expires_at"`
			}{SessionID: response.JSON200.SessionId, ExpiresAt: response.JSON200.ExpiresAt.String()}
			return renderSafeValue(command.OutOrStdout(), bundle.config.output, safe)
		},
	}
	command.Flags().Var(&challengeID, challengeID.name, "Device challenge identity")
	return command
}

func readBoundedSecret(reader io.Reader) (string, error) {
	if reader == nil {
		return "", errors.New("device proof stdin is unavailable")
	}
	document, err := io.ReadAll(io.LimitReader(reader, maximumInputLength+2))
	if err != nil || len(document) == 0 || len(document) > maximumInputLength+1 {
		return "", errors.New("device proof stdin must be bounded")
	}
	document = bytes.TrimSuffix(document, []byte{'\n'})
	document = bytes.TrimSuffix(document, []byte{'\r'})
	if len(document) == 0 || len(document) > maximumInputLength || bytes.ContainsAny(document, "\r\n") {
		return "", errors.New("device proof stdin must contain exactly one non-empty line")
	}
	return string(document), nil
}
