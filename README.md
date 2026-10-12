# souvenir

A small, self-hosted personal AI chat application with persistent memory and
searchable history. It talks to any OpenAI-compatible endpoint, so the same
binary works against a local `llama.cpp` server, OpenRouter, Mistral or OpenAI.

Written in Go to keep the footprint small: one static binary, with Postgres and
`pgvector` as the only dependency. No separate vector database.

## Status

Pre-1.0 and under active development. The chat loop is usable day to day, and
hybrid search over past conversations and memories is in place.

## What it does

### Chat

Requests go to `/v1/chat/completions` on whatever backend you point it at, so
switching providers is a config change rather than a code change. `/v1/models`
lists what the server is offering, and you can switch between them from inside
the app without restarting.

Responses stream. The SSE reader in `llm/streamclient.go` turns `data:` lines
into events on a channel, and the UI renders tokens as they arrive. Esc cancels
a reply mid-stream and keeps what has arrived so far. Content and
reasoning are separate events, so models that emit `reasoning_content` can have
their thinking shown apart from the answer. Tool call fragments are accumulated
by index across chunks and assembled into a complete message at the end.

Tool calls run in a loop: the calls are dispatched locally, their results
appended, and the model called again, up to `Llm.MaxToolRounds` rounds. The
rounds are working state for the turn and are shown faded above the answer, but
only the final reply is saved, so a reopened conversation sends exactly what a
live one does; the cost is that the model reruns a lookup it needs again later.
Tools live in a registry mapping a name to a Go function and a JSON schema, and
return a plain string so the packages behind them never import the API types.
Esc cancels a running tool.

`web_search` queries the configured search backend (SearXNG for now) and returns the top results' titles,
links and snippets, optionally limited to the last day, week, month or year. It
is offered only when `WebSearch.Url` is set. `web_fetch` reads a page: the main
content is extracted with readability and converted to markdown, and long pages
come in `Fetch.MaxChars` parts the model asks for in turn. Only http and https
are fetched, and every connection, redirects included, is checked on the
address actually dialled, so private, loopback, link-local and other non-public
ranges are refused even behind a public name.

`search_history` runs the same hybrid search as the `/history` search over every
other conversation and returns the matching messages with their conversation
and date, so the model can look up what was said before.

Memory is a separate table of durable facts ("The user is vegetarian"), not
transcript. The model curates it with `memory_save`, `memory_search`,
`memory_update` and `memory_forget`: recalled memories carry their ids, so a fact
the user refines ("since September 2025") is rewritten in place rather than
saved again, and saving the same text twice returns the existing memory.
Memories get the same per-model vector tables and hybrid search as messages,
are embedded as soon as they are saved, and are backfilled by the background
pass if that failed. Unlike message search, memory search needs embeddings: it
returns an error rather than falling back to keywords.

On every message the top `Llm.MemoryRecall` memories (default 5, 0 turns it
off) are found with that search and given to the model ahead of the
conversation. Like tool rounds they are working state for the turn: shown faded
above the answer, never saved into the history.

Conversations get a title and a summary from a single cheap-model call, using a
separate model if you configure one. It runs in the background after the first
reply, and never replaces a title set with `/rename <title>`.

Long conversations are kept inside `Llm.ContextBudget` tokens (estimated at
four characters each) with a rolling summary. When a reply pushes the context
over budget, everything but the last `Llm.KeepRecent` messages is folded, along
with the previous summary, into a new one by the title model, in the background.
Summaries live in their own table, one row per "messages 1..n", and the model
then gets the latest summary plus the messages after it. The `messages` table is
untouched, so history and search still see every message. A budget of 0 turns
summarizing off.

### Terminal UI

Bubble Tea, with a chat view, a landing screen, and a slash-command palette with
fuzzy matching. The commands are `/exit`, `/history`, `/memories`, `/models`,
`/new` and `/rename`. `/memories` lists what the assistant remembers, with the
same `/` search and `x` to forget as the history browser. The history browser reopens any past conversation and picks it back
up. `/` in it searches every conversation by keyword and by meaning, Esc
clearing the search, and `x` (or Delete) on a conversation deletes it, with its messages, summaries
and search index, after a confirmation, since it cannot be undone. `/rename` asks the title model for a title, and `/rename <title>` sets one
by hand.

Text pastes with the terminal's own paste (Shift+Insert, Ctrl+Shift+V) and with
Ctrl+V. Run locally, Ctrl+V also takes an image (PNG, JPEG, WebP or GIF, up to
10 MB) from the clipboard through `wl-paste` or `xclip`. It is attached to the
next message, shown above the input, and Esc removes it. Over SSH, Ctrl+V asks
the terminal for its clipboard (OSC 52), which carries text only, so images
cannot be pasted there. The chat model has to accept images; one that does not
fails the turn.

### Storage

Postgres, via `pgx`. The app creates its own database and schema on first run.

Conversations and messages are stored with an ordering sequence per
conversation, and saves are incremental: only messages past the stored maximum
sequence get written. Every message carries a generated `tsvector` column with a
GIN index, which gives keyword search for free and is half of the hybrid
search.

Pasted images are stored with their message in `message_images` and sent
again whenever the conversation is, so a reopened conversation still has them.

Messages are split into overlapping word chunks and stored with a hash of their
content, which is what makes re-embedding idempotent.

### Embeddings

An OpenAI-compatible `/v1/embeddings` client with batching. Vectors go into a
table named after the embedding model and its dimension, created on demand.

Finding work to do is a single query: left join the chunks against the vector
table on `content_hash` and take the rows that miss. New messages and a switch
to a different embedding model both fall out of that one query, and re-running
it only does what is left, so it is safe to interrupt.

Embedding runs on a timer in the background rather than during a turn. It picks
up conversations that have gone quiet and have chunks without vectors, so the
chat path never waits on it.

### Configuration

A JSON file, `.config.json`, with `SOUV__*` environment variables overriding any
key. It covers the API endpoint, the chat and title models, the database
connection, the embedding backend and chunking, and logging, which supports
several handlers at once with their own level, format and target.

### Search

`db/search` runs hybrid search: cosine distance over the vector table and
`websearch_to_tsquery` over the text index, merged with reciprocal rank fusion,
falling back to keywords alone when embeddings are unavailable. Keyword search
catches exact names that embeddings paraphrase away, vectors catch restatements
that share no words. Search ranks messages rather than titles, since titles are
generated, and backs both the `/history` search and the `search_history` tool.
Only user and assistant messages are indexed; tool rounds are never saved, so a
fetched page cannot turn up in every later search. Exact search is fine at this
scale; an ANN index only starts earning its keep somewhere north of 100k
chunks.

### Users

Conversations and memories belong to a user. The user travels in the request
context (`users.WithUser`), and every store method reads it through one
helper, `users.Id`, and filters by it; a call without a user is refused rather
than seeing everything. Messages, chunks, summaries and images are reached only
through a conversation the user owns. Both legs of hybrid search filter by user
before their candidate limit, so a nearest-neighbour query cannot fill its
quota with someone else's rows and come back empty. Another user's conversation
or memory is reported as not found. The same memory text can be saved by two
users. Each user also has their own calendars (see Calendars).

The migration that added users created `owner`, an admin who inherited
everything written before. The local TUI runs as the owner, and the last admin
cannot be removed.

## Planned

Roughly the order things are expected to land in.

### Retrieval

Context assembly keeps the last few turns verbatim, a rolling summary of
everything older and the recalled memories (see Chat above). Still to come: the
most relevant chunks pulled from earlier in the same conversation.

### Tools

Tools are exposed through the API's native tool calling.
MCP would only be worth the transport layer if these tools needed to be
reachable from other clients, and they do not, since they live in the same
binary.

### Calendar

Not yet covered by the calendar tools: to-dos
(`VTODO`), changing a single occurrence of a recurring event (skipping one is
covered by exceptions), and moving an event between calendars.

### On the web

Not on the web yet, though the TUI has them: deleting conversations, searching history, and reviewing memories. Also still
to come: regenerating a reply, and trusting the client address from the proxy's
header only when the request came through it.

### Logins

SSH keys and OIDC sign-ins exist. Still to come: a local password, and signing
out at the provider too (RP-initiated logout).

Organizations and teams are deliberately left for later.

## Configuration

`.config.json` is read from the working directory. Any string, number or
boolean key, or list of strings (comma separated), can be overridden with a
`SOUV__section__key` environment variable (`SOUV__llm__model`); names are
matched case-insensitively, and an unknown key is a startup error rather than
silently ignored. `Logging` is a list and can only be set in the file.

Each `Logging` entry is a handler with its own level, format (`text` or `json`)
and target: a file path, `stdout`, `stderr` or `discard`. The TUI owns the
terminal, so an entry without a target writes to `souvenir.log`.

```json
{
  "Api":       { "Url": "http://localhost:11435", "Key": "", "Timeout": 300000 },
  "Llm":       { "Model": "...", "TitleModel": "...", "Thinking": false,
                 "MaxToolRounds": 28, "ContextBudget": 12000, "KeepRecent": 8,
                 "MemoryRecall": 5 },
  "Db":        { "Url": "localhost", "Port": "5432", "DbName": "souvenir",
                 "User": "...", "Password": "...",
                 "ChunkSize": 400, "ChunkOverlap": 40 },
  "Embedding": { "Url": "...", "Key": "", "Model": "...", "Dim": 1024,
                 "BatchSize": 64, "Timeout": 60000,
                 "Interval": 60, "Quiet": 300, "MaxDistance": 0.72, "DistanceMargin": 0.15,
                 "QueryPrefix": "", "DocPrefix": "" },
  "Logging":   [{ "Level": "debug", "Format": "json", "Target": "log.log" }]
}
```

### Web search

```json
"WebSearch": { "Provider": "searxng", "Url": "http://searxng:8080", "Timeout": 10000 }
```

`Provider` picks the backend behind `web_search`; `searxng` (the default) is
the only one so far, and an unknown one stops startup. A new backend implements
`web.Searcher` and gets a case in `web.New`. For `searxng`, `Url` is an
instance with the `json` format enabled under `search.formats` in its
`settings.yml` (otherwise it answers 403), and its limiter off or this client
allowed. Left out, `web_search` is not offered. `Timeout` is in milliseconds.

### Fetch

```json
"Fetch": { "Enabled": true, "Timeout": 20000, "MaxBytes": 5242880, "MaxChars": 20000 }
```

On by default. `Timeout` is in milliseconds, `MaxBytes` caps what is downloaded
and `MaxChars` how much of a page the model gets per call. HTML, text, JSON and
XML are read; anything else is refused.

### Calendars

```json
"Calendars": { "Path": "$HOME/.calendars/icloud", "Read": ["home", "work"],
               "Write": ["assistant"], "Timezone": "Europe/Brussels" }
```

`Path` is the base directory, holding one directory per user, named after the
user: `<Path>/<user>/<calendar>/<event>.ics`. Each user's directory is a
`vdirsyncer` filesystem storage: one subdirectory per calendar, one `.ics` file
per event. The tools only ever see the directory of the user they run for, and
a user name that is not a plain directory name is refused. A user without a
directory is not offered the calendar tools at all. `Read` and
`Write` name the calendars within each user's directory, the same for everyone.
Renaming a user (`souvenir users rename`) means renaming their directory too.
Environment variables in `Path` are expanded (`~` is not), and it must exist at
startup or the calendar tools are disabled. The app never speaks CalDAV; it reads and writes those
files and leaves syncing to `vdirsyncer`. Only calendars named in `Read` or
`Write` are visible to the model, and every tool call is checked against them
again: `Read` allows `calendar_list` (a day, Monday-to-Sunday week or month,
recurring events expanded), `calendar_search` and `calendar_get`; `Write` also
allows `calendar_create`, `calendar_update` and `calendar_delete`, and implies
read access. A repeating event takes an iCalendar RRULE, which can end on
a date (`UNTIL`) or after a number of occurrences (`COUNT`), and a list of
dates it skips, written as `EXDATE`s at each occurrence's exact start. With no
`Read` or `Write` entries the tools are not offered.
The lists can be set from the environment as comma separated values
(`SOUV__calendars__read=home,work`).

`Timezone` is the IANA zone the model's times are read and shown in, and the
zone new events are written with (plus a matching `VTIMEZONE`). Left out, the
process's local zone is used and new events are written in UTC. The same zone
is used for the current date and time the model is given on every turn.

`Llm.Thinking` sets llama.cpp's `enable_thinking` template argument. Leave it
out for other servers: nothing server specific is sent unless it is set.

`Embedding.Dim` is required and must match the model's output size.
`Interval` is how often, in minutes, the background pass looks for work, and
`Quiet` is how many minutes a conversation must go untouched before it is
embedded. `MaxDistance` drops vector matches above that cosine distance (0
turns the cutoff off), and `DistanceMargin` drops those further than that
from the best match, since short messages such as "hi" sit at a similar
distance from every query. The right values depend on the model. `QueryPrefix` and
`DocPrefix` are prepended to search queries and stored chunks for models that
expect instructions; Qwen3-Embedding wants a query prefix such as
`"Instruct: Given a search query, retrieve relevant passages\nQuery: "` and
no document prefix. Changing `DocPrefix` does not re-embed existing chunks.

The two model settings behave differently, which is worth knowing before you
change one.

The chat model floats. Switch it whenever you like, including at runtime with
`/models`.

The embedding model is pinned for the life of the store. Changing it does not
migrate or destroy anything, it just starts filling a new table, because a
384-dimension column cannot hold a 768-dimension vector. Going from one model to
another and back does not re-embed anything. While a new table is still filling,
keyword search keeps working, so search gets worse rather than breaking.

### Web frontend

The chat on the web shares the TUI's core (the `agent` package) and its data: a
conversation started in one shows up in the other. Pages are rendered on the
server and htmx swaps them in; a small `app.js` handles the reply stream, Enter
to send, copy and stop. Everything, htmx included, is embedded in the binary and
served from it, so the pages run under a Content-Security-Policy allowing only
the server's own scripts and styles.

Sending saves the message and starts the reply on the server, detached from the
request, then streams it to the page as server-sent events: the thinking, the
memories recalled and tool calls made, and the answer rendered from markdown
(raw HTML in it is dropped). Closing the page does not stop the reply, and
opening the conversation again picks the stream back up. One reply runs per
conversation at a time. When it is finished the conversation is named and, past
its budget, summarized, as in the TUI. The model picker lists the provider's
models and remembers the choice.

Long titles end in an ellipsis, with the full title as a tooltip. The pencil
next to a conversation in the sidebar, or next to the open one's title, renames
it in place: Enter or leaving the field saves, Escape cancels. A title you set
is never replaced by a generated one, and renaming does not move the
conversation up the list. It waits until a reply has finished, since the reply
may still name the conversation.

Images pasted into the message box, or dropped on it, are attached to the
message: up to four, of at most 10 MB each, shown as thumbnails until sent.
Pasting a clipboard that also holds text, such as a copy from a document,
pastes the text instead. The server decides each image's type from its bytes
and accepts only PNG, JPEG, WebP and GIF. Images are saved with the message, as
in the TUI, and need a chat model that reads them.

### Web sign-in

Setting `Web.Listen` makes `souvenir serve` also serve the web frontend, where
people sign in with an OpenID Connect provider such as Authentik.

```json
"Web":  { "Listen": ":8080", "BaseUrl": "https://llm.example.com", "SessionDays": 30 },
"Oidc": { "Issuer": "https://auth.example.com/application/o/souvenir/",
          "ClientId": "...", "ClientSecret": "...", "MatchClaim": "preferred_username" }
```

In Authentik, create an OAuth2/OpenID provider (confidential client) with the
redirect URI `<BaseUrl>/auth/callback` and the `openid`, `profile` and `email`
scopes, and an application for it. The issuer is the provider's "OpenID
Configuration Issuer", trailing slash included; it must match exactly.

Sign-in uses the authorization code flow with PKCE, a nonce, and a state that is
also kept in a cookie, so a callback only completes in the browser that started
it, and only once. The ID token's signature, issuer, audience and expiry are
checked. The provider is first contacted on the first sign-in, so the server
starts even while it is down.

Who may sign in is decided in Authentik, by binding a group or policy to the
application; souvenir has no sign-up of its own. The first time an unknown login
appears, its `MatchClaim` claim is compared with the user names. A user of that
name who has no login from this provider yet is linked to it; otherwise a new,
non-admin user of that name is created and linked. From then on only the
provider's issuer and subject count, so renaming the account at the provider
changes nothing. A user has at most one login per provider, so a second provider
account claiming a taken name is refused rather than taking the user over.
Matching on `email` is allowed only when the provider says the address is
verified. To sign in to the existing data, rename the owner to your provider
user name first (`souvenir users rename owner NAME`). `souvenir users unlink
NAME` forgets a user's links and ends their sessions.

Sessions live in Postgres, and only a hash of each token is stored. The cookie
is `HttpOnly` and `SameSite=Lax`; over https it is `Secure` with the `__Host-`
prefix. Sign-out is a POST accepted only from `BaseUrl`'s own origin. Every page
is sent with a strict Content-Security-Policy and `no-store`.

## Running

Needs Go 1.26.8 or newer and a reachable Postgres with the `vector` extension
available. The database and tables are created on first run.

```sh
go run .          # the TUI in this terminal, as the owner
go run . serve    # the TUI over SSH

go run . users                      # list users
go run . users add NAME [admin]     # add a user
go run . users remove NAME          # remove a user and everything they own
go run . users rename NAME NEW      # rename a user
go run . users unlink NAME          # forget their web sign-ins, end their sessions
go run . keys                       # list registered SSH keys
go run . keys add NAME < id.pub     # register a key (or pass it after NAME)
go run . keys remove SHA256:...     # remove a key
```

`serve` hosts the same TUI over SSH with [wish](https://github.com/charmbracelet/wish),
one session per connection, all sharing the database and the background
embedding. A key registered with `keys add` logs in as its user. A key listed in
`Ssh.AuthorizedKeys` (default `authorized_keys`, in OpenSSH format) logs in as
the owner; the file is read on each login, so deleting a line revokes it. Set
it to `""` to allow registered keys only; when set, it refuses to start
without the file. Any other key is refused. The
host key is generated on first start at `Ssh.HostKeyPath` (default
`.ssh/souvenir_ed25519`) and must persist, or clients will see it change.

```sh
ssh -p 23234 localhost
```

```json
"Ssh": { "Listen": ":23234", "HostKeyPath": ".ssh/souvenir_ed25519",
         "AuthorizedKeys": "authorized_keys" }
```

The database tests run against a real Postgres when `SOUVENIR_TEST_DB` is set;
they create and drop databases with that prefix, on the server named by the
usual `PGHOST`, `PGPORT`, `PGUSER` and `PGPASSWORD`:

```sh
SOUVENIR_TEST_DB=souvenir_test PGUSER=me go test ./...
```

## Deploying

Pushing a `v*` tag (`git tag v0.1.0 && git push --tags`) runs
`.gitea/workflows/release.yml`: it builds the image, pushes it to the Gitea
registry as `radumaco/souvenir:<version>`, and commits that version to the
`souvenir-deploy` repository, whose own workflow redeploys. The
compose file, the secrets and the one-time setup live there.

The image runs `souvenir serve` as uid 1000, configured entirely through
`SOUV__*` variables, logging to stdout, with its SSH host key and
`authorized_keys` under `/data`.

The release needs the repository secrets `REGISTRY` (the Gitea host),
`REGISTRY_USER`, `REGISTRY_TOKEN` and `DEPLOY_REPO_TOKEN` (write access to
souvenir-deploy).

## License

See [LICENSE](LICENSE).
