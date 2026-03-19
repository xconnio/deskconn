# Deskconn

Deskconn is a desktop management suite.
It consists of two main tools: a desktop daemon and a command-line client.

## Installation
```bash
curl -fsSL https://get.deskconn.com | sh
```

## deskconnd

The desktop daemon responsible for registering and exposing desktop APIs to the Deskconn ecosystem. It runs in the
background and enables remote management and control of the desktop.

### Run

```bash
make run-deskconnd
```

## deskconnctl

A command-line tool used to manage desktops. It can attach a desktop to the cloud, execute commands, and perform other
administrative tasks.

### Build

```bash
make build-deskconn
```

### Run

Run the resulting binary to see available commands and help:

```bash
./deskconn
```
