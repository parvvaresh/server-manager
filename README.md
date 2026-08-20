# sshm

A terminal manager for SSH and SFTP servers, written in Go with no external
dependencies. It keeps a list of your servers, lets you search them as you
type, and opens a connection on the one you pick. Several servers can be
opened at once, side by side, in a tiled tmux window.

The vault is a plain tab separated file, and it can be encrypted in place with
AES-256. The encrypted form is byte compatible with
`openssl enc -aes-256-cbc -pbkdf2`, so the vault stays readable by anything
else that speaks that format.

## Features

- Store name, host, port, user, password, key path and tags for each server
- Interactive UI with incremental search
- Open `ssh` or `sftp` on the selected server
- Mark several servers and open them all in one tiled tmux window
- Install your public key on a server with `copy-id`
- Encrypt and decrypt the vault with a master password
- Import and export the vault as TSV

## Requirements

- Go 1.21 or newer, to build
- An `ssh` client, and `sftp` if you want SFTP
- `tmux`, only for opening several servers at once
- `sshpass`, optional; when it is missing, password logins fall back to an
  askpass helper

## Install

Build and install to `/usr/local/bin`:

```
make install
```

Choose a different prefix:

```
make install PREFIX=$HOME/.local
```

Build without installing; the binary lands in `bin/sshm-go`:

```
make build
```

Install straight from the module path:

```
go install github.com/parvvaresh/server-manager/cmd/sshm@latest
```

## Usage

Run with no arguments to open the interactive UI:

```
sshm
```

Everything the UI does is also available as a subcommand:

| Command | What it does |
| --- | --- |
| `sshm add [NAME USER@HOST[:PORT] [PASS] [TAGS]]` | Add a server, from arguments or by prompting field by field |
| `sshm ls` | List every stored server |
| `sshm edit NAME` | Edit a server |
| `sshm rm NAME` | Delete a server |
| `sshm ssh NAME` | Connect with `ssh` |
| `sshm sftp NAME` | Connect with `sftp` |
| `sshm multi NAME NAME ...` | Open all of them in a tiled tmux window |
| `sshm copy-id NAME [KEYFILE]` | Install your public key on the server |
| `sshm lock` | Encrypt the vault with a master password |
| `sshm unlock` | Decrypt the vault back to plain text |
| `sshm export [--with-passwords]` | Write the vault as TSV to standard output |
| `sshm import FILE.tsv` | Merge servers from a TSV file, skipping duplicates |
| `sshm where` | Print the path of the vault currently in use |
| `sshm version` | Print the version |
| `sshm help` | Show the built in help |

### Examples

Add a server and connect to it:

```
sshm add web1 root@203.0.113.10:22 "" production,web
sshm ssh web1
```

Open three servers side by side:

```
sshm multi web1 web2 db1
```

Back the vault up, passwords included, and restore it elsewhere:

```
sshm export --with-passwords > backup.tsv
sshm import backup.tsv
```

Encrypt the vault once you are done editing it:

```
sshm lock
```

## Where the vault lives

The vault directory is the first of these that is set:

1. `$SSHM_HOME`
2. `$XDG_CONFIG_HOME/sshm`
3. `~/.config/sshm`

Inside it, `servers.tsv` is the plain vault and `servers.enc` is the encrypted
one. Only one of the two exists at a time; `sshm lock` replaces the first with
the second and `sshm unlock` does the reverse. The plain file is written with
mode `600`. Run `sshm where` if you are unsure which one is in use.

## Environment

| Variable | Effect |
| --- | --- |
| `SSHM_HOME` | Directory holding the vault |
| `SSHM_SSH_OPTS` | Extra flags passed to `ssh`, for example `-o StrictHostKeyChecking=accept-new` |
| `XDG_CONFIG_HOME` | Used to derive the default vault directory |

## Security notes

Passwords are stored in the vault so that connections can be opened without
typing them. While the vault is unlocked they sit on disk in plain text,
protected only by the `600` file mode, so keep the vault encrypted when you are
not using it. Key based authentication avoids the problem entirely: add a key
path to a server, or run `sshm copy-id` to put your public key on the remote
host, and leave the password field empty.

## Development

```
make fmt
make vet
make build
```

Continuous integration runs the same formatting check, `go vet` and a build on
every push and pull request, and cross compiles for Linux and macOS on both
amd64 and arm64. Pushing a tag that starts with `v` publishes those binaries as
a GitHub release.
