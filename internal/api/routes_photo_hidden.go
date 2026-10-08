package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

const photoHiddenCookie = "docbank-hidden"

type PhotoHiddenPasscodeRequest struct {
	Passcode    string `json:"passcode,omitempty" maxLength:"1024"`
	NewPasscode string `json:"new_passcode,omitempty" maxLength:"1024"`
}

type photoHiddenOutput struct {
	SetCookie string `header:"Set-Cookie"`
	Body      store.PhotoHiddenState
}

func hiddenCookie(token string, expires time.Time) string {
	cookie := &http.Cookie{Name: photoHiddenCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 300, Expires: expires}
	if token == "" {
		cookie.MaxAge = -1
	}
	return cookie.String()
}

func hiddenError(err error) error {
	var lockout *store.HiddenLockoutError
	if errors.As(err, &lockout) {
		return huma.ErrorWithHeaders(NewError(http.StatusTooManyRequests, "hidden_lockout", lockout.Error()), http.Header{"Retry-After": {strconv.Itoa(max(1, int(time.Until(lockout.Until).Seconds())))}})
	}
	return FromStoreError(err)
}

func photoHiddenMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(photoHiddenCookie); err == nil {
			r = r.WithContext(store.WithPhotoHiddenToken(r.Context(), cookie.Value))
		}
		if len(r.URL.Path) >= len("/api/v1/photos/") && r.URL.Path[:len("/api/v1/photos/")] == "/api/v1/photos/" {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Vary", "X-Api-Key, Authorization, Cookie, "+WebSessionHeader)
		}
		next.ServeHTTP(w, r)
	})
}

func registerPhotoHiddenRoutes(api huma.API, d Deps, g *gate, snapshots *store.QuerySnapshotService) {
	huma.Register(api, huma.Operation{OperationID: "getPhotoHiddenState", Method: http.MethodGet, Path: "/api/v1/photos/hidden", Summary: "Read hidden photos configuration and current unlock expiry"}, func(ctx context.Context, _ *struct{}) (*photoHiddenOutput, error) {
		state, err := d.Store.PhotoHiddenState(ctx)
		return &photoHiddenOutput{Body: state}, hiddenError(err)
	})
	for _, action := range []string{"setup", "change", "disable", "unlock", "lock", "reset"} {
		huma.Register(api, huma.Operation{OperationID: action + "PhotoHidden", Method: http.MethodPost, Path: "/api/v1/photos/hidden/" + action, Summary: action + " hidden photos access", MaxBodyBytes: 16 << 10}, func(ctx context.Context, in *struct{ Body PhotoHiddenPasscodeRequest }) (*photoHiddenOutput, error) {
			var token string
			var expiry time.Time
			err := g.mutate(func() error {
				switch action {
				case "setup":
					return d.Store.SetupPhotoHidden(ctx, in.Body.Passcode)
				case "change":
					return d.Store.ChangePhotoHidden(ctx, in.Body.Passcode, in.Body.NewPasscode)
				case "disable":
					return d.Store.DisablePhotoHidden(ctx, in.Body.Passcode)
				case "reset":
					return d.Store.ResetPhotoHidden(ctx)
				case "lock":
					return d.Store.LockPhotoHidden(ctx)
				case "unlock":
					var err error
					token, expiry, err = d.Store.UnlockPhotoHidden(ctx, in.Body.Passcode)
					return err
				}
				return nil
			})
			if err != nil {
				return nil, hiddenError(err)
			}
			if action != "unlock" {
				snapshots.Revoke("")
			}
			state, err := d.Store.PhotoHiddenState(store.WithPhotoHiddenToken(ctx, token))
			if err != nil {
				return nil, hiddenError(err)
			}
			out := &photoHiddenOutput{Body: state}
			if action == "unlock" || action == "lock" || action == "change" || action == "disable" || action == "reset" {
				out.SetCookie = hiddenCookie(token, expiry)
			}
			return out, nil
		})
	}
	for _, action := range []string{"hide", "unhide"} {
		huma.Register(api, huma.Operation{OperationID: action + "PhotoAsset", Method: http.MethodPost, Path: "/api/v1/photos/assets/{asset_id}/" + action, Summary: action + " a revisioned photo asset"}, func(ctx context.Context, in *struct {
			AssetID string `path:"asset_id"`
			IfMatch string `header:"If-Match"`
		}) (*photoAssetOutput, error) {
			revision, err := parseIfMatch(in.IfMatch)
			if err != nil {
				return nil, err
			}
			var asset store.PhotoAsset
			err = g.mutate(func() error {
				var err error
				asset, err = d.Store.SetPhotoAssetHidden(ctx, in.AssetID, revision, action == "hide")
				return err
			})
			if err != nil {
				return nil, hiddenError(err)
			}
			snapshots.Revoke("")
			return &photoAssetOutput{ETag: revisionETag(asset.Revision), Body: fromStorePhotoAsset(asset)}, nil
		})
	}
}
