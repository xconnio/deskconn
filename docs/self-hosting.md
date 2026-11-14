# Self-hosting

Run your own cloud (Hub, management service, web app and database) locally with the Docker Compose setup in
[`docker/`](../docker), then build `deskconn` pointed at it.

### 1. Start the stack

```bash
cd docker
make setup
make run
cd ..
```

### 2. Build the binaries against it

```bash
make build CLOUD_QUIC_ADDRESS=127.0.0.1:8081   # builds bin/deskconn and bin/desk
```

`CLOUD_QUIC_ADDRESS` is the Hub's QUIC address, baked in as the default. For a stack on another machine, use its
`host:port`. Local addresses (`127.0.0.1`, `localhost`, `::1`) skip TLS verification, because the local Hub uses a
self-signed certificate; any other host needs a Hub certificate the client trusts.

Setting `DESKCONN_CLOUD_QUIC_ADDRESS` at runtime overrides the built-in address without rebuilding:

```bash
DESKCONN_CLOUD_QUIC_ADDRESS=127.0.0.1:8081 desk ls
```

WebRTC uses a public STUN server only. To also relay through your own TURN server, set it in the environment.
`DESKCONN_TURN_URL` accepts a comma-separated list:

```bash
export DESKCONN_TURN_URL=turn:turn.example.com:3478 DESKCONN_TURN_USERNAME=<user> DESKCONN_TURN_PASSWORD=<password>
```

The deskconn-userd service doesn't inherit your shell's environment; add the variables to it with
`systemctl --user edit deskconn-userd`:

```ini
[Service]
Environment=DESKCONN_TURN_URL=turn:turn.example.com:3478
Environment=DESKCONN_TURN_USERNAME=<user>
Environment=DESKCONN_TURN_PASSWORD=<password>
```

### 3. Create an account and attach

Sign up at http://localhost:3000/register. The stack prints one-time passwords to its logs instead of emailing them
(the Compose service and images still use the old `account-service` name):

```bash
docker compose logs -f account-service   # in docker/
```

Then follow [Getting started](../README.md#getting-started) from step 2 with the binaries from `bin/`, and run the daemon in the
foreground:

```bash
systemctl --user stop deskconn-userd   # if the official release is installed; it shares ~/.deskconn
bin/desk machine attach --username <email>
bin/desk serve
```

Don't run `desk self update` on these builds: it installs the official release, which connects to
`api.deskconn.com`.

