# Adding an app

Apps beyond deskconn-userd's own (Files, Terminal, Resource Monitor, Text Editor) run as their own processes, like
[deskconn-media](https://github.com/xconnio/deskconn-media) and
[deskconn-remote-control](https://github.com/xconnio/deskconn-remote-control).
An app needs no code in deskconn: it connects to deskconn-userd's local socket, registers its procedures there, and
tells deskconn-userd which apps it serves. deskconn-userd lists them to clients (the web app shows an app only while
it's listed and enabled), owners and admins enable or disable them (`desk app enable|disable`), and xconn exposes
their procedures over routed and direct connections.

1. **Connect** to the local daemon with `appkit.Connect` (realm `io.xconn.deskconn.local`).
   This uses a Unix socket, or authenticated loopback yamux on Windows.
   Reconnect when deskconn-userd restarts.
2. **Register a key exchange.** deskconn only supports end-to-end encrypted apps: deskconn-userd refuses an app without one.
   Clients call it with their X25519 public key and get the app's; both sides derive their keys with HKDF-SHA256,
   `frontendToBackend` for requests and `backendToFrontend` for results. Every other procedure takes its request as a
   single encrypted JSON argument (nonce + ChaCha20-Poly1305 ciphertext, empty or absent when there is none) and returns
   a single encrypted JSON result. Keys are per caller session.
3. **Register the apps** by calling `io.xconn.local.apps.register` with a JSON list:

   ```json
   [{"id": "pictures", "name": "Pictures", "key_exchange": "io.xconn.media.key.exchange",
     "requires": ["files"], "default_off": true}]
   ```

   `requires` are apps this one can't work without (it's disabled with them), and a `default_off` app stays disabled
   until an owner or admin enables it. Ids must be unique on the machine. The apps are listed until the app's session
   leaves.
4. **Follow enabled apps.** Call `io.xconn.deskconn.capabilities.list` for every app's
   `{"id", "name", "enabled", "key_exchange", "requires"}`, and subscribe to
   `io.xconn.deskconn.capabilities.on_change`, published with the same list whenever an app is enabled or
   disabled. Register an app's procedures while it's enabled and unregister them while it's disabled; keep the key
   exchange registered.

Procedures under `io.xconn.deskconn.` are reachable by remote clients (and `io.xconn.deskconn.admin.` by
the machine's owner and admins only), and so are the app prefixes the Hub allows (`relay.AppPrefixes`:
`io.xconn.remotecontrol.` and `io.xconn.media.`), so the key exchange and the app's procedures go under one of them.
Everything else on the socket, such as `io.xconn.local.*`, stays local.

In Go, the `github.com/xconnio/deskconn/appkit` package has what an app needs: the connection (`appkit.Connect`), the
procedures above, and `appkit.Encryption`, which does step 2:

```go
session, err := appkit.Connect(ctx, cfgDirectory)
enc := appkit.NewEncryption()
if err := enc.Register(session, "io.xconn.media.key.exchange"); err != nil {
    return err
}
handler := enc.Encrypted(func(ctx context.Context, req []byte) (any, error) {
    // req is the decrypted JSON request; the result is sent back as encrypted JSON.
    return map[string]string{"hello": "world"}, nil
})
resp := session.Register("io.xconn.media.hello", handler).Execute()
```

A client finds the app's key exchange in the apps list, then calls it before the app's procedures:

```go
keys, err := appkit.ClientKeyExchange(session, keyExchange)
result, err := appkit.EncryptedCall(session, "io.xconn.media.hello", nil, keys)
```

### Manifest apps

An app that only needs a UI over procedures other apps already serve can be a manifest app instead: a directory of
YAML with an `app.yaml` at its root, which clients render with
[deskconn-app-engine](https://github.com/xconnio/deskconn-app-engine). It runs no process of its own. Owners and admins
register it on a machine, where it's stored in `~/.deskconn/manifests` and listed, enabled and disabled like any
other app:

```bash
desk app register office ./remote-control
desk app ls office
desk app unregister office remote-control
```

