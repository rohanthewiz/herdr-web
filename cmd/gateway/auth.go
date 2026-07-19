//go:build ghostty

package main

import (
	"html"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/herdr-web/internal/gwauth"
)

// resolveSecret returns the shared access secret: the --password flag, else
// the HERDR_PASSWORD env, else a freshly generated one (generated=true so the
// caller logs it for the operator to use).
func resolveSecret(flagVal string) (secret string, generated bool, err error) {
	if flagVal != "" {
		return flagVal, false, nil
	}
	if env := os.Getenv("HERDR_PASSWORD"); env != "" {
		return env, false, nil
	}
	secret, err = gwauth.GenerateSecret()
	return secret, true, err
}

// authGuard enforces WS10 access control for the gateway: an unauthenticated
// browser is bounced to /login, where it exchanges the shared secret for an
// HMAC-signed session cookie; a headless client presents the secret as a
// bearer token. The WebSocket upgrade additionally requires a same-origin
// request. A nil *authGuard means auth is disabled (--auth none) and no
// middleware is installed.
type authGuard struct {
	a      *gwauth.Authenticator
	secure bool // set the session cookie Secure (server is serving TLS)
}

// middleware gates every request. Public paths (/login, /favicon.ico) pass
// through; everything else needs a valid session cookie or bearer token, and
// /ws also needs a same-origin Origin. Browser navigations without auth are
// redirected to /login; API/WS calls get a 401 so they fail fast.
func (g *authGuard) middleware(ctx rweb.Context) error {
	path := ctx.Request().Path()
	if path == "/login" || path == "/favicon.ico" {
		return ctx.Next()
	}
	if path == "/ws" {
		origin := ctx.Request().Header("Origin")
		if !gwauth.OriginOK(origin, ctx.Request().Host()) {
			return ctx.Status(http.StatusForbidden).WriteText("forbidden: cross-origin websocket")
		}
	}
	if g.authed(ctx) {
		return ctx.Next()
	}
	if path == "/ws" {
		return ctx.Status(http.StatusUnauthorized).WriteText("unauthorized")
	}
	return ctx.Redirect(http.StatusFound, "/login")
}

// authed reports whether the request carries valid credentials: a bearer token
// matching the shared secret, or a valid session cookie.
func (g *authGuard) authed(ctx rweb.Context) bool {
	if g.a.CheckBearer(ctx.Request().Header("Authorization")) {
		return true
	}
	if cookie, err := ctx.GetCookie(gwauth.CookieName); err == nil {
		return g.a.ValidSession(cookie, time.Now())
	}
	return false
}

// handleLoginGet renders the login form (already authenticated → straight to
// the app).
func (g *authGuard) handleLoginGet(ctx rweb.Context) error {
	if g.authed(ctx) {
		return ctx.Redirect(http.StatusFound, "/")
	}
	return ctx.WriteHTML(loginPage(""))
}

// handleLoginPost checks the submitted password, and on success issues the
// session cookie and redirects to the app. Failures re-render the form with a
// 401 so a probe can distinguish them.
func (g *authGuard) handleLoginPost(ctx rweb.Context) error {
	form, _ := url.ParseQuery(string(ctx.Request().Body()))
	if !g.a.CheckSecret(form.Get("password")) {
		return ctx.Status(http.StatusUnauthorized).WriteHTML(loginPage("Incorrect password."))
	}
	cookie := &rweb.Cookie{
		Name:     gwauth.CookieName,
		Value:    g.a.IssueSession(time.Now()),
		Path:     "/",
		MaxAge:   int(g.a.TTL() / time.Second),
		HttpOnly: true,
		Secure:   g.secure,
		SameSite: rweb.SameSiteStrictMode,
	}
	if err := ctx.SetCookieWithOptions(cookie); err != nil {
		return ctx.Status(http.StatusInternalServerError).WriteText("failed to set session")
	}
	return ctx.Redirect(http.StatusSeeOther, "/")
}

// loginPage renders the login form, optionally with an error banner. The page
// is self-contained (no external assets) so it works before any auth.
func loginPage(errMsg string) string {
	banner := ""
	if errMsg != "" {
		banner = `<p class="err">` + html.EscapeString(errMsg) + `</p>`
	}
	return `<!DOCTYPE html>
<html lang="en"><head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>herdr · sign in</title>
<style>
  html,body{margin:0;height:100%;background:#181818;color:#d4d4d4;
    font-family:ui-monospace,"SF Mono",Menlo,Consolas,monospace;
    display:flex;align-items:center;justify-content:center;}
  form{background:#202020;border:1px solid #333;border-radius:8px;padding:28px 26px;
    width:300px;box-shadow:0 4px 20px rgba(0,0,0,.5);}
  h1{font-size:16px;margin:0 0 4px;color:#e8e8e8;}
  p.sub{font-size:12px;color:#888;margin:0 0 18px;}
  label{display:block;font-size:12px;color:#aaa;margin:0 0 6px;}
  input{width:100%;box-sizing:border-box;padding:9px 10px;font-size:14px;
    background:#141414;color:#e8e8e8;border:1px solid #3a3a3a;border-radius:5px;
    font-family:inherit;}
  input:focus{outline:none;border-color:#5b9dff;}
  button{margin-top:16px;width:100%;padding:9px;font-size:14px;cursor:pointer;
    background:#2f68c8;color:#fff;border:none;border-radius:5px;font-family:inherit;}
  button:hover{background:#3a78e0;}
  p.err{color:#ff6b6b;font-size:12px;margin:0 0 14px;}
</style></head><body>
<form method="post" action="/login">
  <h1>herdr gateway</h1>
  <p class="sub">Enter the access password to continue.</p>
  ` + banner + `
  <label for="password">Password</label>
  <input id="password" name="password" type="password" autofocus autocomplete="current-password"/>
  <button type="submit">Sign in</button>
</form>
</body></html>`
}
