# souvenir

A small, self-hosted personal AI chat application with persistent memory and
searchable history. It talks to any OpenAI-compatible endpoint, so the same
binary works against a local `llama.cpp` server, OpenRouter, or OpenAI.

Written in Go to keep the footprint small: one static binary, with Postgres and
`pgvector` as the only dependency. No separate vector database.

## Status

Pre-1.0 and under active development. The chat loop is usable day to day. The
retrieval side, which is the whole point of the name, is still being built.

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

`search_history` runs the same hybrid search as the `/history` search over every
other conversation and returns the matching messages with their conversation
and date, so the model can look up what was said before.

Memory is a separate table of durable facts ("The user is vegetarian"), not
transcript. The model curates it with `memory_save`, `memory_search` and
`memory_forget`; saving the same text twice returns the existing memory.
Memories get the same per-model vector tables and hybrid search as messages,
are embedded as soon as they are saved, and are backfilled by the background
pass if that failed.

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

### Storage

Postgres, via `pgx`. The app creates its own database and schema on first run.

Conversations and messages are stored with an ordering sequence per
conversation, and saves are incremental: only messages past the stored maximum
sequence get written. Every message carries a generated `tsvector` column with a
GIN index, which gives keyword search for free and is half of the eventual
hybrid retrieval.

Messages are split into overlapping word chunks and stored with a hash of their
content, which is what makes re-embedding idempotent.

### Embeddings

An OpenAI-compatible `/v1/embeddings` client with batching. Vectors go into a
table named after the embedding model and its dimension, created on demand.

Finding work to do is a single query: left join the chunks against the vector
table on `content_hash` and take the rows that miss. New messages, edited
messages and a switch to a different embedding model all fall out of that one
query, and re-running it only does what is left, so it is safe to interrupt.

Embedding runs on a timer in the background rather than during a turn. It picks
up conversations that have gone quiet and have chunks without vectors, so the
chat path never waits on it.

### Configuration

A JSON file, `.config.json`, with `SOUV__*` environment variables overriding any
key. It covers the API endpoint, the chat and title models, the database
connection, the embedding backend and chunking, and logging, which supports
several handlers at once with their own level, format and target.

## Where it stops

`db/search` runs hybrid search: cosine distance over the vector table and
`websearch_to_tsquery` over the text index, merged with reciprocal rank fusion,
falling back to keywords alone when embeddings are unavailable. It backs
the `/history` search and the `search_history` tool, but nothing feeds retrieved context
into a prompt automatically yet.
On every message the top `Llm.MemoryRecall` memories (default 5, 0 turns it
off) are found with the same search and given to the model ahead of the
conversation. Like tool rounds they are working state for the turn: shown faded
above the answer, never saved into the history.

## Planned

Roughly the order things are expected to land in.

### Retrieval

This is the hard part and the centerpiece, so it gets the most time.

Search combines both signals: cosine distance over the vector table merged and
reranked with `to_tsquery` over the text index. Keyword search catches exact
names that embeddings paraphrase away, vectors catch restatements that share no
words. Exact search is fine at this scale; an ANN index only starts earning its
keep somewhere north of 100k chunks.

Cross-conversation search ranks on message content rather than titles, since
titles are generated and unreliable, and returns the matching conversation with
a snippet. It shows up both as a search box in the UI and as a tool the model
can call when it decides an old conversation is relevant.

Context assembly already keeps the last few turns verbatim and a rolling summary
of everything older (see Chat above). Still to come: the most relevant chunks
pulled from earlier in the same conversation, and relevant memories.
Tool output is skipped or marked when indexing, because a fetched web page will
otherwise turn up in recall for every term it happens to mention. Queries and
documents get different prefixes where the model expects them, which is easy to
miss and quietly costs recall.

### Memory

Memory is in place (see Chat above).

### Tools

The loop and `search_history` are in place (see Chat above); memory, web and
calendar tools follow. Tools are exposed through the API's native tool calling. MCP would only be worth
the transport layer if these tools needed to be reachable from other clients,
and they do not, since they live in the same binary.

### Web access

Search against a SearXNG instance, then a separate fetch tool, because search
snippets are too thin to answer from. Fetched pages go through readability
extraction and markdown conversion and get truncated to a budget before they
reach the model.

Fetch needs an SSRF guard: http and https only, private, loopback and
link-local ranges rejected, and the resolved address rechecked on every redirect
hop, since redirecting into the internal network is the obvious bypass.

### Calendar

Reading and writing iCalendar files in a directory that `vdirsyncer` keeps in
sync. The app never speaks CalDAV itself, which keeps calendar support down to
parsing and writing files.

### Other frontends

The TUI is served over SSH (`souvenir serve`); for now every authorized key is
the same single user. Still to come: a small embedded web frontend sharing the same core. Both reachable from outside the
home network behind a reverse proxy, with the origin locked to the proxy and
client addresses trusted from a header only when the request actually came
through it.

### Multiple users

One user identity with credentials attached to it. A local password, an OIDC
login and an SSH key are all just credentials pointing at the same user, so
linking two logins is linking two credentials rather than a special case.

OIDC uses the authorization code flow with PKCE and full ID token validation.
Accounts link on the issuer and subject pair, never on email, since email can be
reassigned. Matching on a claim happens once when an unknown login first
appears, with a uniqueness check so an ambiguous match cannot become an account
takeover. Sessions live in Postgres so they can be revoked.

Every owned row carries a user id and every access decision goes through one
scoping helper rather than being open-coded across dozens of queries. Vector
search needs particular care here: the user filter has to apply before the
limit, or a nearest-neighbour query happily returns someone else's rows.
Organizations and teams are deliberately left for later.

## Configuration

`.config.json` is read from the working directory. Any string, number or
boolean key can be overridden with a `SOUV__section__key` environment variable,
nested sections included (`SOUV__db__history__enabled`); names are matched
case-insensitively, and an unknown key is a startup error rather than silently
ignored. `Logging` is a list and can only be set in the file.

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

## Running

Needs Go 1.26.8 or newer and a reachable Postgres with the `vector` extension
available. The database and tables are created on first run.

```sh
go run .          # the TUI in this terminal
go run . serve    # the TUI over SSH
```

`serve` hosts the same TUI over SSH with [wish](https://github.com/charmbracelet/wish),
one session per connection, all sharing the database and the background
embedding. Only keys listed in `Ssh.AuthorizedKeys` (default `authorized_keys`,
in OpenSSH format) can connect, and it refuses to start without that file. The
host key is generated on first start at `Ssh.HostKeyPath` (default
`.ssh/souvenir_ed25519`) and must persist, or clients will see it change.

```sh
ssh -p 23234 localhost
```

```json
"Ssh": { "Listen": ":23234", "HostKeyPath": ".ssh/souvenir_ed25519",
         "AuthorizedKeys": "authorized_keys" }
```

## License

See [LICENSE](LICENSE).
