# Deskconn Docker

Runs the full Deskconn stack locally from the published Docker Hub images (`latest` tag):

| Service           | Image                                     | Host ports                                      |
|-------------------|-------------------------------------------|-------------------------------------------------|
| `postgres-init`   | `xconnio/deskconn-account-service:latest` | — (copies the Postgres init script, then exits) |
| `postgres`        | `postgres:16`                             | —                                               |
| `router`          | `xconnio/deskconn-router:latest`          | 8081/udp, 8082/udp, 8083, 8084                  |
| `migrate`         | `xconnio/deskconn-account-service:latest` | — (runs migrations once, then exits)            |
| `account-service` | `xconnio/deskconn-account-service:latest` | —                                               |
| `web-app`         | `xconnio/deskconn-web-app:latest`         | 3000                                            |

Works on Linux, macOS and Windows ([Docker Desktop](https://docs.docker.com/desktop/)).

Run every command below from this `docker/` directory unless noted otherwise.

## Setup

Pulls the images and creates `.env` from `example.env` (an existing `.env` is kept):

```shell
make setup
```

On Windows, `make` isn't installed by default. In PowerShell:

```powershell
docker compose pull
if (!(Test-Path .env)) { Copy-Item example.env .env }
```

## Environment variables

Set in `.env`. Every value has a working default.

| Variable                              | Default                    | Description                                                               |
|---------------------------------------|----------------------------|---------------------------------------------------------------------------|
| `POSTGRES_PASSWORD`                   | `postgres`                 | Password of the Postgres superuser.                                       |
| `ACCOUNT_SERVICE_DB_USER`             | `account-service`          | Database user of the account service.                                     |
| `ACCOUNT_SERVICE_DB_PASSWORD`         | `account-service-password` | Password of the account service database user.                            |
| `ROUTER_DB_USER`                      | `router`                   | Database user of the router (read-only).                                  |
| `ROUTER_DB_PASSWORD`                  | `router-password`          | Password of the router database user.                                     |
| `X_DEBUG`                             | `true`                     | Print OTPs to the logs instead of emailing them.                          |
| `RESEND_API_KEY`                      | empty                      | [Resend](https://resend.com) API key; required when `X_DEBUG=false`.      |
| `DESKCONN_ACCOUNT_PRIVATE_KEY`        | local-only key             | Private key the account service authenticates to the router with.        |
| `DESKCONN_ACCOUNT_SERVICE_PUBLIC_KEY` | local-only key             | Public key the router accepts; must match `DESKCONN_ACCOUNT_PRIVATE_KEY`. |

Database users are created only on the first start. After changing them, reset the data (see [Stop / reset](#stop--reset)).

## Run

```shell
make run
```

On Windows (PowerShell):

```powershell
docker compose up -d
```

Open http://localhost:3000/login in Chrome or Edge (the local router uses a self-signed certificate, which the browser
accepts through WebTransport certificate hashes). If Windows Firewall asks about Docker opening ports, allow it.

Migrations run automatically on every run. With `X_DEBUG=true`, OTPs are printed to the logs:

```shell
docker compose logs -f account-service
```

## Connect your desktop

To manage desktops through this stack instead of `api.deskconn.com`, build `deskconn` and `desk` with the router's
QUIC address (port 8081):

```shell
make build CLOUD_QUIC_ADDRESS=127.0.0.1:8081   # in the repo root
```

See [Self-hosting](../README.md#self-hosting) in the main README for the full steps.

## Update

Pulls the newest `latest` images, then restarts on them:

```shell
make pull
make run
```

On Windows: `docker compose pull`, then `docker compose up -d`.

## Stop / reset

```shell
make down                # stop, keep data
docker compose down -v   # stop and delete the database
```

On Windows: `docker compose down` to stop, `docker compose down -v` to also delete the database.

## Notes

- The host ports above clash with the per-repo `docker compose` setups; stop those first.
