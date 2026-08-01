# iparent

`iparent` is a small Go prototype for a parent-admin challenge and rewards app.

## Run

Initialize the local environment and SQLite database:

```sh
iparent init
```

For local development without installing the binary:

```sh
go run . init
```

This creates:

- `config/.env`
- `data/main.sqlite`
- `data/uploads/`

Running `iparent init` again will keep the existing `config/.env` and make sure the database and upload folders exist.

Start the app:

```sh
iparent
```

For local development:

```sh
go run .
```

Then open `http://localhost:8097`.

`iparent` uses port `8097` by default. The port is documented in `config/.env`:

```env
PORT=8097
```

For a one-off run on a different port, start the app with `PORT=8098 go run .`.

The default admin credentials are written to `config/.env`:

```env
ADMIN_USERNAME=admin
ADMIN_PASSWORD=change-me-now
```

Change the default password before using the app beyond local testing.

## Prototype Scope

- Single parent admin from `config/.env`
- Signed HTTP-only cookie sessions
- SQLite login-failure ledger with IP rate limiting
- Child account creation from the admin portal
- Challenge creation for multiple choice, select all, true/false, number, short answer, long answer, and photo proof
- Manual review for long-answer/photo challenges and optional manual review for any challenge
- First successful earning attempt awards points; repeat submissions do not award more points unless the admin unlocks the child again
- Basic reward creation and display
