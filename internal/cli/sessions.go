package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

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
		Long: "Read one device proof line from stdin, exchange it for a PM session, and store the session. " +
			"The command reads stdin until end-of-file. Close stdin after the proof line. " +
			"Without --wait=true, the command sends one exchange request. " +
			"With --wait=true, the command repeats the request while the server answers " +
			"device_authorization_pending or device_poll_too_soon. " +
			"Before each repeat, the command waits for the Retry-After time of the server answer. " +
			"The --request-timeout value limits the whole wait.",
		Args: noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if !challengeID.set {
				return usageFailure(errors.New("--challenge-id is required"))
			}
			proof, err := readDeviceProof(command.Context(), command.InOrStdin())
			if err != nil {
				return err
			}
			ctx, cancel, bundle, err := runtime.requestBundle(command, false, false)
			if err != nil {
				return err
			}
			defer cancel()
			// Select the session store and its key before the first exchange
			// request. A store that cannot be selected then stops the command
			// before the server consumes the challenge.
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
			pause := runtime.options.devicePollPause
			if pause == nil {
				pause = pauseForDevicePoll
			}
			session, err := exchangeDeviceProof(ctx, bundle.client, deviceExchange{
				request: pmapi.DeviceProof{ChallengeId: challengeID.value, Proof: proof},
				wait:    bundle.config.wait,
				notices: command.ErrOrStderr(),
				pause:   pause,
			})
			if err != nil {
				return err
			}
			// The server has consumed the challenge. A store failure, or a
			// cancellation or the deadline during the store, cannot be repaired
			// by another exchange. The backend Set cannot be stopped after it
			// starts.
			if err := store.Store(ctx, key, session); err != nil {
				return commandExit(err, "device login exchange succeeded, but the CLI did not store the PM session. "+
					"Start a new device login with pm sessions login-start.")
			}
			safe := struct {
				SessionID string `json:"session_id"`
				ExpiresAt string `json:"expires_at"`
			}{SessionID: session.SessionId, ExpiresAt: session.ExpiresAt.String()}
			return renderSafeValue(command.OutOrStdout(), bundle.config.output, safe)
		},
	}
	command.Flags().Var(&challengeID, challengeID.name, "Device challenge identity")
	return command
}

const (
	devicePollMinimumSeconds = 1
	devicePollMaximumSeconds = 60
	// devicePollSlowDown is added after device_poll_too_soon, as RFC 8628
	// section 3.5 asks for slow_down. The server stays the cadence authority.
	devicePollSlowDown = 5 * time.Second
)

type deviceExchange struct {
	request pmapi.DeviceProof
	wait    bool
	notices io.Writer
	pause   func(context.Context, time.Duration) error
}

// exchangeDeviceProof sends the device proof. Without --wait it sends one
// request. With --wait it repeats only after device_authorization_pending and
// device_poll_too_soon, and only at the cadence that the server states in
// Retry-After. The caller's context is the one deadline for all requests and
// waits. The proof stays in memory and goes into no output or error.
func exchangeDeviceProof(ctx context.Context, client *pmapi.QualifiedClient, exchange deviceExchange) (pmapi.PMDeviceSession, error) {
	for {
		if err := ctx.Err(); err != nil {
			return pmapi.PMDeviceSession{}, deviceWaitStopped(err)
		}
		response, err := client.API().ExchangeDeviceLoginWithResponse(ctx, exchange.request)
		if err != nil {
			// The request may have reached the server, and a successful
			// exchange consumes the challenge. A repeat cannot succeed then
			// and hides the real failure.
			return pmapi.PMDeviceSession{}, exchangeOutcomeUnknown(requestError(ctx, err))
		}
		envelope, err := pmapi.DecodeEnvelope(response)
		if err != nil {
			return pmapi.PMDeviceSession{}, exchangeOutcomeUnknown(err)
		}
		problem := pmapi.EnvelopeError(envelope)
		if problem == nil {
			if response.JSON200 == nil {
				return pmapi.PMDeviceSession{}, exchangeOutcomeUnknown(pmapi.ErrMalformedEnvelope)
			}
			return *response.JSON200, nil
		}
		if !exchange.wait {
			return pmapi.PMDeviceSession{}, problem
		}
		delay, repeat, err := devicePollDelay(*envelope.Problem, response.HTTPResponse)
		if err != nil {
			return pmapi.PMDeviceSession{}, err
		}
		if !repeat {
			return pmapi.PMDeviceSession{}, problem
		}
		if deadline, bounded := ctx.Deadline(); bounded && !time.Now().Add(delay).Before(deadline) {
			// The last answer was a definite 409 or 429, so the challenge is
			// not consumed and a new run with a longer limit is safe.
			return pmapi.PMDeviceSession{}, commandExit(context.DeadlineExceeded,
				"device login wait stopped. The next exchange request does not fit in the --request-timeout limit. "+
					"Run the command again with a longer --request-timeout.")
		}
		_, _ = fmt.Fprintf(exchange.notices,
			"notice: device login exchange received %s. The CLI waits %ds before the next exchange request.\n",
			envelope.Problem.Value.Code, delay/time.Second)
		if err := exchange.pause(ctx, delay); err != nil {
			return pmapi.PMDeviceSession{}, deviceWaitStopped(err)
		}
	}
}

// deviceWaitStopped reports a context that ended before the next exchange
// request. No request was sent, so the challenge state did not change.
func deviceWaitStopped(err error) error {
	if errors.Is(err, context.Canceled) {
		return commandExit(err, "device login was cancelled before the next exchange request. "+
			"The CLI sent no request after the cancellation.")
	}
	return err
}

// devicePollDelay returns the wait before the next exchange request and
// whether the answer allows one. Only 409 device_authorization_pending and 429
// device_poll_too_soon allow it, and only with exactly one Retry-After value
// of 1 to 60 whole seconds. The client never guesses an interval.
func devicePollDelay(problem pmapi.DecodedProblem, response *http.Response) (time.Duration, bool, error) {
	var slowDown time.Duration
	switch {
	case problem.Value.Status == http.StatusConflict && problem.Value.Code == "device_authorization_pending":
	case problem.Value.Status == http.StatusTooManyRequests && problem.Value.Code == "device_poll_too_soon":
		slowDown = devicePollSlowDown
	default:
		return 0, false, nil
	}
	malformed := commandExit(pmapi.ErrMalformedEnvelope,
		"device login wait stopped. The server answer did not have exactly one Retry-After value from 1 to 60 seconds.")
	if response == nil {
		return 0, false, malformed
	}
	values := response.Header.Values("Retry-After")
	if len(values) != 1 {
		return 0, false, malformed
	}
	seconds, err := strconv.Atoi(values[0])
	if err != nil || strconv.Itoa(seconds) != values[0] ||
		seconds < devicePollMinimumSeconds || seconds > devicePollMaximumSeconds {
		return 0, false, malformed
	}
	return time.Duration(seconds)*time.Second + slowDown, true, nil
}

func pauseForDevicePoll(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func exchangeOutcomeUnknown(err error) error {
	return commandExit(err, "device login exchange outcome is unknown. "+
		"The exchange request may have reached the server, but the CLI received no valid answer. "+
		"Start a new device login with pm sessions login-start.")
}

// commandExit keeps the exit code of the existing classification of err and
// replaces its message with a safe message for the device login phase.
func commandExit(err error, message string) error {
	code, _ := classifyCommandError(err)
	return &exitError{code: code, message: message, cause: err}
}

// readDeviceProof reads the proof once from stdin. The read has no time limit:
// it ends only at end-of-file, so the caller must close stdin. A cancellation
// during the read ends the command at once, before any request. The blocked
// read then ends only when the process exits or stdin closes.
func readDeviceProof(ctx context.Context, reader io.Reader) (string, error) {
	const cancelled = "device login was cancelled while the CLI read the proof from stdin. The CLI sent no exchange request."
	if err := ctx.Err(); err != nil {
		return "", commandExit(err, cancelled)
	}
	type result struct {
		proof string
		err   error
	}
	read := make(chan result, 1)
	go func() {
		proof, err := readBoundedSecret(reader)
		read <- result{proof: proof, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", commandExit(ctx.Err(), cancelled)
	case value := <-read:
		if value.err != nil {
			return "", usageFailure(value.err)
		}
		return value.proof, nil
	}
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
