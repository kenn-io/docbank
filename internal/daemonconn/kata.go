package daemonconn

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/kataclient"
	"go.kenn.io/docbank/internal/kataevidence"
)

func (c *Connection) CiteKataEvidence(ctx context.Context, request kataevidence.CiteRequest) (kataclient.Issue, error) {
	var response *http.Response
	_, err := c.apiWithResponse(&response).CiteKataEvidence(runtime.WithStreamingResponse(ctx), &apiclient.CiteKataEvidenceRequestOptions{Body: &request})
	if err != nil {
		return kataclient.Issue{}, err
	}
	defer func() { _ = response.Body.Close() }()
	var transport struct {
		kataclient.Issue

		Schema string `json:"$schema,omitzero"`
	}
	if err := decodeCitationResponse(response, &transport, 3<<20); err != nil {
		return kataclient.Issue{}, &responseDecodeError{err: err}
	}
	if transport.UID == "" || transport.QualifiedRef == "" {
		return kataclient.Issue{}, &responseDecodeError{err: errors.New("kata issue response is missing its identity; retry creates with the same key")}
	}
	return transport.Issue, nil
}

func (c *Connection) KataContext(ctx context.Context, request kataevidence.ContextRequest) (kataevidence.Context, error) {
	if request.Issue == "" || len(request.Issue) > 256 || request.Offset < 0 || request.Offset > kataevidence.MaxContextOffset {
		return kataevidence.Context{}, kataevidence.ErrInvalid
	}
	var response *http.Response
	_, err := c.apiWithResponse(&response).GetKataContext(runtime.WithStreamingResponse(ctx), &apiclient.GetKataContextRequestOptions{Body: &request})
	if err != nil {
		return kataevidence.Context{}, err
	}
	defer func() { _ = response.Body.Close() }()
	var transport struct {
		kataevidence.Context

		Schema string `json:"$schema,omitzero"`
	}
	if err := decodeCitationResponse(response, &transport, 3<<20); err != nil {
		return kataevidence.Context{}, &responseDecodeError{err: err}
	}
	if transport.Issue.UID == "" || len(transport.Passages) > kataevidence.ContextPageSize || transport.NextOffset != 0 &&
		(transport.NextOffset != request.Offset+len(transport.Passages) || transport.NextOffset > kataevidence.MaxContextOffset) {
		return kataevidence.Context{}, &responseDecodeError{err: errors.New("kata context response is invalid")}
	}
	return transport.Context, nil
}

// KataOfflineAllowed excludes authentication, config and credential-policy failures.
func KataOfflineAllowed(err error) bool {
	if IsTransportError(err) {
		return true
	}
	var network *net.OpError
	return errors.As(err, &network)
}

// ReadKataContextOffline reads the local owner's configured Kata connection only.
// It never opens vault storage or starts a daemon; every valid reference is unreachable.
func ReadKataContextOffline(ctx context.Context, request kataevidence.ContextRequest) (kataevidence.Context, error) {
	layout, err := home.Resolve()
	if err != nil {
		return kataevidence.Context{}, err
	}
	cfg, err := config.Load(layout.Root)
	if err != nil {
		return kataevidence.Context{}, err
	}
	connection, err := cfg.Integrations.Kata.Resolve(layout.Root)
	if err != nil {
		return kataevidence.Context{}, err
	}
	service := kataevidence.Service{Kata: kataclient.New(connection)}
	return service.Context(ctx, request)
}
