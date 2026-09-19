# steam-auth — one-time Steam authentication

Mints a **WebBrowser refresh token** for the Go service through
[`steam-session`](https://github.com/DoctorMcKay/node-steam-session).
Node is needed **only here, only once** — normal service operation is
pure Go and never shells out to Node.

```bash
npm install
npm run steam-auth                 # interactive, human-readable
npm run steam-auth -- --json       # machine-readable JSON on stdout
npm run steam-auth -- --output ./steam-auth.json   # also save to file (0600)
```

Supported logins: username/password, Steam Guard email code
(`--code` or interactive prompt), authenticator app TOTP code, and mobile
approval (waits up to 120s).

Output on success:

```json
{ "steam_id": "7656119...", "refresh_token": "...", "platform": "web" }
```

Configure the service with `STEAM_ID` and `STEAM_REFRESH_TOKEN`.

> **Secret handling.** The refresh token is a password-equivalent: the CLI
> never prints the password, never persists passwords or guard codes, and
> `--output` files are written mode `0600` and git-ignored
> (`steam-auth.json`, `*.steam-auth.json`). If the service later reports
> `ErrSteamReauthenticationRequired`, run this CLI again and replace the
> configured token.
