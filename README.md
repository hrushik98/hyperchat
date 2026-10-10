# Harper

A simple local chat app for DeepSeek, built with **Vue 3** and **Go**. Stream responses, choose your model, and keep conversations in **PostgreSQL through GORM**. **Redis** caches the available model list. Images live in private **SeaweedFS object storage**.

## Start

You need Docker with Docker Compose (Docker Desktop includes both).

1. Add your DeepSeek API key to `.env` in this directory:

   ```dotenv
   DEEPSEEK_API_KEY=your_key_here
   ```

   If you don't have `.env` yet, copy `.env.example`. Your existing `.env` is used as-is.

2. Start Docker, then run:

   ```bash
   ./start.sh
   ```

3. Open **http://localhost:4003**. On the first visit, create your username and master password, then save the recovery key shown on screen. Each self-hosted installation has its own owner account.

The launcher builds the app images, starts all five services (Vue/Nginx, Go, PostgreSQL, Redis, SeaweedFS) in the background, and waits for them to be healthy. Re-running it updates the app and picks up `.env` changes. The Go backend runs on **http://localhost:4004**; its health endpoint is `/api/health` and reports PostgreSQL, Redis, and object storage connectivity.

```bash
./start.sh --logs       # Follow both services' logs
./start.sh --stop       # Stop and remove containers
```

You can also use `docker compose up --build -d --wait` and `docker compose down` directly.

## Your owner account

Harper has one owner per installation. The first-run setup screen asks for a username and a master password of at least 12 characters (maximum 72 UTF-8 bytes). After setup, the login screen asks for those credentials before loading any conversations or models. Existing installations also need this one-time setup after upgrading; their saved chats are preserved. There are no default credentials or additional registrations.

Setup shows a private recovery key once. Copy or download it and keep it somewhere safe. If you forget your username or password, choose **Forgot your username or password?** on the login screen and enter that key with a new username and password. Reset preserves all chats, signs out every old session, and supplies a replacement recovery key. The old key stops working. **Reset account** in the navigation drawer opens the same flow.

Use **Lock workspace** in the navigation drawer to sign out. Sessions expire after seven days and survive backend restarts. Accounts, password hashes, recovery-key hashes, and session-token hashes are stored in PostgreSQL and persist through normal Docker restarts and rebuilds. Passwords use bcrypt; session cookies are HttpOnly and SameSite=Strict. Chat, history, model, import, and image APIs all require a valid session. Login, setup, and reset share a limit of ten attempts per minute per running backend.

The recovery key is required for the reset flow, even if you are currently signed in. It is not stored in browser storage or retrievable from the app. Keep it with your password manager. The password controls access; it does not encrypt database backups.

The default app remains available only on localhost over HTTP. If you separately configure HTTPS hosting, set `AUTH_COOKIE_SECURE=true` in `.env` to require HTTPS for session cookies. The existing backend origin checks accept only the local Harper URLs; a custom-domain deployment would also require explicitly configuring its allowed origin.

## Using Harper

- Choose a model in the composer. The available list is fetched from DeepSeek, so model changes do not require code changes.
- Choose a model marked **Vision**, then use the paperclip, paste an image, or drop images into the composer. Preview and remove attachments before sending. Image-only messages are supported.
- **Think deeper** enables DeepSeek's thinking mode. Expand “Thinking process” to see the reasoning returned by the API.
- Open the menu at the top left to access saved conversations. The plus button in the composer starts a new conversation.
- Press **Enter** to send and **Shift+Enter** for a new line. **Ctrl/Cmd+Shift+O** starts a new conversation.
- Stop a reply, retry a failed request, regenerate the latest answer, or copy a response.
- Use the download icon beneath a reply to export the current chat as Markdown.
- Conversations are saved in PostgreSQL and are available from any browser accessing this local instance. Only your selected chat and model preferences stay in browser storage.
- On the first visit after upgrading, old browser chats are imported automatically. The browser copy is removed only after the import and a database read succeed. Repeated imports do not overwrite existing chats.

## Storage

The Go backend uses GORM models, automatic schema migration, and transactions for every database operation. Messages (including returned thinking text, errors, and partial replies) are stored in a JSONB field on the `conversations` model. The backend saves your prompt before contacting DeepSeek, checkpoints replies while streaming, and commits the final reply before signaling completion. On a backend restart, unfinished replies are marked stopped.

Image bytes are stored in SeaweedFS's private S3-compatible `harper-images` bucket. PostgreSQL stores image metadata and message associations using the GORM `image_attachments` model; transcripts include references to those records. Images are served through `/api/images/{id}` by Go. Object keys and storage credentials stay on the backend. Redis does not store images.

For each image conversation, Go reads the saved objects and sends image bytes inline to DeepSeek, so no public image URL or public bucket is needed. Follow-up questions and regeneration reuse the saved images. Deleting a conversation immediately hides its images and queues object deletion; cleanup retries every 30 seconds if storage is unavailable. Abandoned uploads expire after 24 hours.

Uploads accept JPEG, PNG, GIF, and WebP, up to four images per message, 8 MB per image, and 8192 pixels per side. A conversation can contain up to 12 images and 32 MB of image data. The backend verifies the actual image format and checks the selected model's capabilities. DeepSeek Flash supports image input; Pro currently accepts text only. The picker prevents switching an image conversation to a text-only model.

Docker Compose manages three named volumes:

| Service | Docker volume | Container path |
| --- | --- | --- |
| PostgreSQL 18 | `harperchat_postgres_data` | `/var/lib/postgresql` |
| Redis 8 | `harperchat_redis_data` | `/data` |
| SeaweedFS | `harperchat_object_data` | `/data` |

`./start.sh --stop`, `docker compose down`, and image rebuilds preserve these volumes. `docker compose down --volumes` deletes them, including your chat history and images.

PostgreSQL, Redis, and SeaweedFS are accessible only within the Compose network, without host ports. PostgreSQL uses the `harper` database and user. You can optionally set `POSTGRES_PASSWORD` in `.env` **before the first startup**; the local default is `harper-local-dev`. Changing that variable later does not change an existing database user's password.

Redis runs with append-only persistence and caches the model list for five minutes. Its cache is scoped to the API key and endpoint using a hash; the API key itself is not stored in Redis. The refresh control bypasses the cache. Redis does not store transcripts, and a Redis outage falls back to fetching models directly.

SeaweedFS runs as a single local `weed mini` service. Set `S3_ACCESS_KEY` and `S3_SECRET_KEY` in `.env` to override the local defaults. Its S3 endpoint is `object-storage:8333` inside Docker; no storage port is published on the host.

Create a database backup with:

```bash
docker compose exec -T postgres pg_dump -U harper -d harper -Fc > harper-backup.dump
```

A database dump includes image metadata, but image files require a separate backup of the `harperchat_object_data` volume. Back up PostgreSQL and object storage together while the app is stopped to preserve matching references.

Harper is a single-user local workspace. All browsers using this instance share the same conversations.

The API key stays on the backend. `.env` is excluded from Git and the Docker build context; only the backend container receives the key at runtime. Docker publishes both ports on `127.0.0.1`. The frontend forwards `/api` requests through Nginx, with buffering disabled for streaming. Markdown output is sanitized before rendering.

The app runs locally; **messages and attached images are sent to DeepSeek's hosted API**, and API usage is billed to your DeepSeek account. Harper supports image attachments; it has no web browsing, audio, video, or general document upload feature.

## Development

Use Go 1.26+ and Node.js 24 LTS or newer. The Docker setup does not require either tool to be installed on the host.

For local development, provide `DEEPSEEK_API_KEY`, the PostgreSQL connection variables (`PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`, `PGPASSWORD`, `PGSSLMODE`), `REDIS_ADDR`, and the object storage settings (`S3_ENDPOINT`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_BUCKET`, optional `S3_USE_TLS=true`) in your shell environment. The Go process itself does not read `.env`. Use PostgreSQL/Redis/object storage instances accessible from the host; the Compose services intentionally do not publish database ports. GORM creates or updates the schema on startup.

```bash
cd backend
go run .
```

In another terminal:

```bash
cd frontend
npm ci
npm run dev
```

Vite serves port 4003 and proxies API requests to the backend on port 4004.

```bash
(cd backend && go test -race ./... && go vet ./...)
(cd frontend && npm test && npm run build)
```

Run integration tests against the actual Compose PostgreSQL, Redis, and SeaweedFS services:

```bash
docker compose up -d --wait postgres redis object-storage
docker compose -f docker-compose.yaml -f docker-compose.test.yaml run --build --rm --no-deps backend
```

Storage tests use isolated temporary tables and uniquely named image objects, then remove them afterward. They exercise real uploads, inline vision payloads, previews, follow-ups, regeneration, and deletion. The test override builds a separate image and leaves the running app containers intact.

Account integration tests also use isolated temporary tables. They verify first-run and concurrent setup, login, protected APIs, session persistence and expiry, logout, recovery-key rotation, and chat preservation on reset.

## Troubleshooting

- **Docker is not running:** start Docker Desktop, then run `./start.sh` again.
- **Port already allocated:** stop the other app using 4003 or 4004, then rerun the launcher.
- **Invalid API key:** update `.env` and rerun `./start.sh` to recreate the backend with the new key.
- **Insufficient balance:** add credit in your DeepSeek account, then retry.
- **Connection error:** check your internet connection and use the refresh control. Backend health confirms the local process is running; loading the model list also verifies API access.
- **Conversation too large:** start a new chat. Text requests are capped at 2 MB; image uploads at 34 MB and four files. Conversations are capped at 200 messages; individual messages are capped at 128 KB.
- **Storage unavailable:** check `docker compose ps` and `./start.sh --logs`. Failed browser imports keep the original local copy for retry. Sending is disabled until chat history loads, and save failures are shown rather than silently treated as success.

API implementation references: [Chat API](https://api-docs.deepseek.com/en/), [model discovery](https://api-docs.deepseek.com/api/list-models/), [thinking mode](https://api-docs.deepseek.com/guides/thinking_mode/), [vision](https://api-docs.deepseek.com/guides/vision/), [SeaweedFS](https://github.com/seaweedfs/seaweedfs).
