// Copyright 2023 Gravitational, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package daemon

import (
	"context"
	"io"
	"sync"

	"github.com/gravitational/trace"
	"github.com/gravitational/trace/trail"

	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/mfa"
	api "github.com/gravitational/teleport/gen/proto/go/teleport/lib/teleterm/v1"
	wancli "github.com/gravitational/teleport/lib/auth/webauthncli"
	wantypes "github.com/gravitational/teleport/lib/auth/webauthntypes"
	libmfa "github.com/gravitational/teleport/lib/client/mfa"
)

// mfaPrompt is a tshd implementation of mfa.Prompt that uses the
// tshdEventsClient to propagate mfa prompts to the Electron App.
type mfaPrompt struct {
	cfg          libmfa.PromptConfig
	clusterURI   string
	promptAppMFA func(ctx context.Context, in *api.PromptMFARequest) (*api.PromptMFAResponse, error)
}

// NewMFAPromptConstructor returns a new MFA prompt constructor
// for this service and the given cluster.
func (s *Service) NewMFAPromptConstructor(clusterURI string) func(cfg *libmfa.PromptConfig) mfa.Prompt {
	return func(cfg *libmfa.PromptConfig) mfa.Prompt {
		return s.NewMFAPrompt(clusterURI, cfg)
	}
}

// NewMFAPrompt returns a new MFA prompt for this service and the given cluster.
func (s *Service) NewMFAPrompt(clusterURI string, cfg *libmfa.PromptConfig) *mfaPrompt {
	return &mfaPrompt{
		cfg:          *cfg,
		clusterURI:   clusterURI,
		promptAppMFA: s.promptAppMFA,
	}
}

func (s *Service) promptAppMFA(ctx context.Context, in *api.PromptMFARequest) (*api.PromptMFAResponse, error) {
	if err := s.importantModalSemaphore.Acquire(ctx); err != nil {
		return nil, trace.Wrap(err)
	}
	defer s.importantModalSemaphore.Release()

	return s.tshdEventsClient.PromptMFA(ctx, in)
}

// Run prompts the user to complete an MFA authentication challenge.
func (p *mfaPrompt) Run(ctx context.Context, chal *proto.MFAAuthenticateChallenge) (*proto.MFAAuthenticateResponse, error) {
	runOpts, err := p.cfg.GetRunOptions(ctx, chal)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Depending on the run opts, we may spawn a TOTP goroutine, webauth goroutine, or both.
	spawnGoroutines := func(ctx context.Context, wg *sync.WaitGroup, respC chan<- libmfa.MFAGoroutineResponse) {
		// Fire App goroutine (TOTP).
		wg.Add(1)
		go func() {
			defer wg.Done()

			resp, err := p.promptMFA(ctx, chal, runOpts)
			respC <- libmfa.MFAGoroutineResponse{Resp: resp, Err: err}
		}()

		// Fire Webauthn goroutine.
		if runOpts.PromptWebauthn {
			wg.Add(1)
			go func() {
				defer wg.Done()

				resp, err := p.promptWebauthn(ctx, chal)
				respC <- libmfa.MFAGoroutineResponse{Resp: resp, Err: trace.Wrap(err, "Webauthn authentication failed")}
			}()
		}
	}

	return libmfa.HandleMFAPromptGoroutines(ctx, spawnGoroutines)
}

func (p *mfaPrompt) promptWebauthn(ctx context.Context, chal *proto.MFAAuthenticateChallenge) (*proto.MFAAuthenticateResponse, error) {
	prompt := wancli.NewDefaultPrompt(ctx, io.Discard)
	opts := &wancli.LoginOpts{AuthenticatorAttachment: p.cfg.AuthenticatorAttachment}
	resp, _, err := p.cfg.WebauthnLoginFunc(ctx, p.cfg.GetWebauthnOrigin(), wantypes.CredentialAssertionFromProto(chal.WebauthnChallenge), prompt, opts)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return resp, nil
}

func (p *mfaPrompt) promptMFA(ctx context.Context, chal *proto.MFAAuthenticateChallenge, runOpts libmfa.RunOpts) (*proto.MFAAuthenticateResponse, error) {
	resp, err := p.promptAppMFA(ctx, &api.PromptMFARequest{
		RootClusterUri: p.clusterURI,
		Reason:         p.cfg.PromptReason,
		Totp:           runOpts.PromptTOTP,
		Webauthn:       runOpts.PromptWebauthn,
	})
	if err != nil {
		return nil, trail.FromGRPC(err)
	}
	return &proto.MFAAuthenticateResponse{
		Response: &proto.MFAAuthenticateResponse_TOTP{
			TOTP: &proto.TOTPResponse{Code: resp.TotpCode},
		},
	}, nil
}
