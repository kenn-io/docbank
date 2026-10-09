package daemonconn

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/store"
)

func (c *Connection) PhotoHiddenState(ctx context.Context) (store.PhotoHiddenState, error) {
	state, err := c.API().GetPhotoHiddenState(ctx)
	if err != nil {
		return store.PhotoHiddenState{}, err
	}
	return *state, nil
}

// PhotoHidden performs an authenticated operation and returns its unlock cookie.
func (c *Connection) PhotoHidden(ctx context.Context, action, passcode, next string) (store.PhotoHiddenState, string, error) {
	body := &api.PhotoHiddenPasscodeRequest{Passcode: passcode, NewPasscode: next}
	var response *http.Response
	var state *store.PhotoHiddenState
	var err error
	client := c.apiWithResponse(&response)
	switch action {
	case "setup":
		state, err = client.SetupPhotoHidden(ctx, &apiclient.SetupPhotoHiddenRequestOptions{Body: body})
	case "change":
		state, err = client.ChangePhotoHidden(ctx, &apiclient.ChangePhotoHiddenRequestOptions{Body: body})
	case "disable":
		state, err = client.DisablePhotoHidden(ctx, &apiclient.DisablePhotoHiddenRequestOptions{Body: body})
	case "unlock":
		state, err = client.UnlockPhotoHidden(ctx, &apiclient.UnlockPhotoHiddenRequestOptions{Body: body})
	case "lock":
		state, err = client.LockPhotoHidden(ctx, &apiclient.LockPhotoHiddenRequestOptions{Body: body})
	case "reset":
		state, err = client.ResetPhotoHidden(ctx, &apiclient.ResetPhotoHiddenRequestOptions{Body: body})
	default:
		return store.PhotoHiddenState{}, "", errors.New("unknown hidden photos operation")
	}
	if err != nil {
		return store.PhotoHiddenState{}, "", mutationRequestError(response, err)
	}
	var cookie string
	if action == "unlock" {
		for _, value := range response.Cookies() {
			if strings.HasPrefix(value.Name, "docbank-hidden-") {
				cookie = value.Name + "=" + value.Value
				break
			}
		}
		if cookie == "" {
			return store.PhotoHiddenState{}, "", &responseDecodeError{err: errors.New("unlock response is missing its cookie")}
		}
	}
	return *state, cookie, nil
}

func (c *Connection) SetPhotoAssetHidden(ctx context.Context, id string, revision int64, hidden bool, cookie string) (api.PhotoAsset, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.PhotoAsset{}, errors.New("photo asset ID and positive revision are required")
	}
	var response *http.Response
	var asset *api.PhotoAsset
	var err error
	if hidden {
		asset, err = c.apiWithResponse(&response).HidePhotoAsset(ctx, &apiclient.HidePhotoAssetRequestOptions{PathParams: &apiclient.HidePhotoAssetPath{AssetID: id}, Header: &apiclient.HidePhotoAssetHeaders{IfMatch: new(revisionIfMatch(revision))}}, func(_ context.Context, request *http.Request) error { request.Header.Set("Cookie", cookie); return nil })
	} else {
		asset, err = c.apiWithResponse(&response).UnhidePhotoAsset(ctx, &apiclient.UnhidePhotoAssetRequestOptions{PathParams: &apiclient.UnhidePhotoAssetPath{AssetID: id}, Header: &apiclient.UnhidePhotoAssetHeaders{IfMatch: new(revisionIfMatch(revision))}}, func(_ context.Context, request *http.Request) error { request.Header.Set("Cookie", cookie); return nil })
	}
	return photoMutationResponse(response, asset, err, id)
}
