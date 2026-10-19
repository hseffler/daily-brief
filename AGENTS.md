# Daily Brief Project Summary

## Current State

Agentic daily brief generator in Go. An LLM agent (Mistral API, tool calling)
gathers information from the user's configured sources, writes a concise brief
and delivers it via Telegram. Runs once per invocation, scheduled daily by a
systemd timer. Project layout follows hostmonitor conventions.

## Project Structure

```
.
├── cmd/
│   ├── main.go            # Entry point: flags, wiring, run-once, delivery
│   ├── config.go          # YAML config, defaults, validation
│   ├── mistral.go         # Mistral chat-completions client (tool calling, Content type)
│   ├── agent.go           # Agentic loop: parallel tool execution, max-iter guard
│   ├── tools.go           # Tool type + registry
│   ├── tool_web.go        # fetch_webpage tool (goquery HTML→text, RSS feed support)
│   ├── tool_email.go      # get_new_emails tool (IMAP via go-imap, stateless time window)
│   ├── tool_weather.go    # get_weather_forecast tool (Open-Meteo API)
│   ├── sma.go             # SMA Speedwire client (sunny library, native UDP)
│   ├── tool_sma.go        # get_pv_status tool
│   ├── vw.go              # VW EU Data Act portal client (evcc's eudataact approach, ported)
│   ├── tool_ev.go         # get_ev_status tool
│   ├── tv.go              # ARD/ZDF program API client (heute-journal/tagesthemen check)
│   ├── tool_tv.go         # get_tv_news_schedule tool
│   ├── sureflap.go        # gosure library adapter (cat flap, inside/outside)
│   ├── tool_pets.go       # get_pet_status tool
│   ├── telegram.go        # Telegram delivery (chunked, 429 backoff)
│   └── *_test.go          # Unit/integration tests (httptest + fakes)
├── Makefile               # Build automation (overridable GOOS/GOARCH, root guards)
├── README.md              # User documentation
├── AGENTS.md              # This file (project summary for contributors)
├── daily-brief.service    # systemd unit (Type=oneshot)
├── daily-brief.timer      # systemd timer (daily 05:00, Persistent=true)
├── daily-brief.yaml.example  # Example configuration
├── go.mod                 # Go module definition (github.com/hseffler/daily-brief)
└── go.sum                 # Go dependencies
```

## Key Features

### 1. Agentic loop with tools
- `Agent.Run` sends the conversation (system prompt built from the configured
  sources + user prompt) to Mistral `chat/completions` with `tools`,
  `tool_choice=auto` and `parallel_tool_calls=true`.
- Requested tool calls are executed **in parallel** when they have different
  names (they are independent); same-name calls are serialized per name,
  because a tool may keep per-call state. Results
  are appended as `role=tool` messages in request order.
- `finish_reason: "length"` (token limit) on a final message logs a warning —
  the possibly truncated brief is still delivered.
- The loop ends when the model replies without tool calls; `max_iterations`
  (default 12) guards against infinite loops. Tool failures become error text
  so the model can react instead of the run dying. Each tool call is bounded
  by a 90s timeout and results are truncated at 12000 chars.

### 2. Sources (all configured in YAML, never hard-coded)
- **Web** (`fetch_webpage`): HTTP GET with a browser User-Agent, charset
  transcoding (`x/net/html/charset`), HTML→readable text via goquery (drops
  script/style/nav/footer/..., keeps `<article>` headings+teasers, capped at
  `web_fetch.max_chars`, default 8000). **RSS feeds** (detected by `<item>`
  elements) and **Atom feeds** (detected by `<entry>` elements) are rendered
  as a simple bullet list of headlines (`- Title`). This is how zeit.de
  (bot-protected on the main site), taz and WELT are read — via their RSS
  feeds on separate domains.
- **Weather** (`get_weather_forecast`): Open-Meteo forecast API (free, no API
  key; uses DWD ICON models for Central Europe). Returns per-day: WMO weather
  code mapped to text, min/max temperature, precipitation amount and
  probability max, and sunshine duration (seconds converted to hours). The
  brief renders this as a compact 7-day monospace table inside `<pre>` tags
  with weather emojis.
- **Email** (`get_new_emails`): Posteo IMAP over TLS (`posteo.de:993`,
  go-imap v1). Mailbox opened **read-only**. Two-phase UID FETCH: metadata
  first, full bodies only for messages ≤ 512 KiB. Snippets come from the
  first `text/plain` MIME part with quoted-printable/base64 decoding
  (stdlib `net/mail` + `mime/multipart`). New-mail detection is
  **stateless**: IMAP `SEARCH SINCE now - email.since_hours` (default 24h,
  a rolling window matching the daily 05:00 schedule) — no state file, no
  dedupe; a delayed or repeated run may re-report mails from the window.
  Posteo requires an **app password**. The fetch limit is `max(4 x
  max_messages, 100)` and the brief cap (`max_messages`) keeps the newest
  mails of the window. `FetchNew` runs the blocking go-imap work in a
  goroutine and abandons it on ctx timeout (go-imap v1 calls are not
  cancellable; the leak is bounded by the run-once process).
- **SMA PV** (`get_pv_status`): **SMA Speedwire (UDP :9522) only**, using the
  same library as evcc — `gitlab.com/bboehmke/sunny` v0.17.0 (Apache-2.0).
  No external exporter. Two device families: battery-capable inverters
  (Sunny Boy Storage, Sunny Island, Tripower Smart Energy, plain
  Sunny Boy/Tripower) read actively with the user password (Speedwire login,
  default `0000`, max 12 chars); SMA Energy Meter / Sunny Home Manager 2.0
  report via Speedwire multicast broadcast (no login). The device type is
  auto-detected (`device.IsEnergyMeter()`); with no configured host, devices
  are discovered on the LAN (`SimpleDiscoverDevices`, optionally filtered by
  serial). Value mapping in `smaStatusFromValues` (pure, unit-tested): sunny
  IDs `PvPower` (hybrids; plain inverters fall back to `ActivePowerPlus`,
  their current AC output — energy meters never report PV power),
  `BatteryCharge` (SOC %), `BatteryVoltage` ×
  `BatteryCurrent` (battery power, evcc's derivation), `BatteryTemperature`,
  `GridPowerImport`/`GridPowerExport` (inverters) or `ActivePowerPlus` /
  `ActivePowerMinus` (energy meters), `ActiveEnergyPlusTodayKWh` (yield
  today), `PvEnergyTotalKWh`/`ActiveEnergyPlusKWh` (total yield),
  `BatteryEnergyChargeKWh`/`BatteryEnergyDischargeKWh`,
  `GridEnergyImportKWh`/`GridEnergyExportKWh` (energy meters: raw Ws ÷
  3600000). Grid power is only reported when the device actually measures it
  (`HasGridPower`): plain inverters without an Energy Meter get an n/a line.
- **TV** (`get_tv_news_schedule`): **ARD (Das Erste) and ZDF** evening news via
  the broadcasters' public program APIs (no account): ARD
  `https://programm-api.ard.de/program/api/program?day=YYYY-MM-DD` (channel
  `daserste`, serves at most 8 days ahead) and ZDF
  `https://api.zdf.de/cmdm/epg/broadcasts?tvServices=zdf&from=…&to=…&page=N`
  (paginated, limit 100; authenticated with the public bearer token ZDF's own
  web player ships — overridable via `sources.tv.zdf_api_key`). The tool checks
  the coming day (the current day in `sources.tv.timezone`, default
  Europe/Berlin): it collects the **heute journal** (ZDF) and **tagesthemen**
  (ARD) editions and reports which start within the configured window
  (`sources.tv.window_start`/`window_end`, default 21:15–21:30, inclusive).
  Editions starting before 06:00 are ignored — the small hours carry overnight
  repeats of the previous evening (e.g. the ~00:20 heute journal repeat),
  which belong to the previous program day. The short late-night bulletin
  "heute journal update" (aired on some days, e.g. Mondays at 23:50) is a
  different program and is not matched as heute journal. A failing source does
  not lose the other; the output carries per-source error lines, and the tool
  only fails when both sources fail. The brief section renders the editions as
  a compact
  monospace `<pre>` block (ready to copy and paste, like the weather table),
  one line per edition: start time · channel · show, with a trailing ⏰ on
  the edition inside the window; when nothing is in the slot, a line after
  the block suggests re-watching **heute (19:00)** in the ZDF Mediathek.
- **EV** (`get_ev_status`): **VW group vehicles (ID.3, e-Golf, e-Up, Audi,
  Skoda, Seat, Cupra)** via the **EU Data Act portal**
  (`https://eu-data-act.drivesomethinggreater.com`) — the same source as
  evcc's `drivesomethinggreater` vehicle type (`vehicle/vw/eudataact` in the
  evcc repo), since VW retired third-party WeConnect API access. There is no
  standalone Go library for the portal; `vw.go` is a minimal port of evcc's
  client, adapted to the run-once model (no dataset store/merge — the newest
  content dataset is read, walking back up to 3 datasets when it lacks the
  SoC). Login is the OIDC flow against `identity.vwgroup.io` driven as an
  HTML form login (email step, password step via the `window._IDK` JS block,
  redirect chain with a cookie jar, marketing-consent page skipped via its
  callback); the portal API authenticates via the session cookie only, with
  one re-login on 401/403. The portal is not live telemetry: it stores a
  zipped JSON dataset roughly every 15 minutes, only while the vehicle is
  active, and the user must enable the data request once in a browser
  (consent + link vehicle + continuous request). Reading vehicle data means
  listing datasets (`/proxy_api/euda-apim/datadelivery/.../list`), downloading
  the newest content dataset(s) (skipping `*_no_content_found.zip`
  placeholders) and decoding the flat data-point list (`{key, dataFieldName,
  value, timestampUtc}`). SoC for the ID.3 is the data point with GUID
  `506cb83e-f99f-3af3-bbeb-0429b69a78d9` (`battery_state_report.soc`);
  fallbacks `battery_level_HV.value`, `state_of_charge`, `hv_soc` (same
  priority as evcc). Range (ID.3 GUID `0ca40e18-...`), charge status (evcc's
  StatusA/B/C mapping) and odometer come from the same dataset. The tool
  output always states the age of the data.
- **Pets** (`get_pet_status`): **SureFlap cat flap (Connect line: Cat Flap
  Connect + Sure Petcare Hub)** via the **Sure Petcare cloud API**, using the
  [gosure](https://github.com/perbu/go-sure) Go library (BSD-3-Clause).
  Sure Petcare publishes no official developer API; the library logs in with
  the account credentials (`POST /api/auth/login` → bearer token) and reads
  `GET /api/pet?with[]=photo&with[]=position&with[]=status` — each pet's
  `position.where` (1 = inside, 2 = outside) and `position.since` (when it
  last changed). The client is wrapped behind the `petFetcher` interface
  (fakes in tests). The location is tracked by the flap (every passage
  updates it; it can be overridden manually in the app) — a cat that goes
  through a window or door is not accounted for. Library quirks: no context
  support (a fixed 30s HTTP timeout bounds the call), no re-login on an
  expired token, and a new random device id per client (each run registers a
  new device on the account, visible in the app).

### 3. Delivery
- Brief printed to stdout and sent to Telegram (bot API `sendMessage` with
  `parse_mode=HTML`, so the brief's `<b>` section headers render bold and
  `<pre>` blocks render as monospace),
  split into ≤4000-rune chunks (rune-safe, newline-preferred, never
  splitting inside an HTML tag), one retry with the server-advised
  `retry_after` on HTTP 429, and a plain-text fallback (tags stripped, no
  parse mode) when Telegram answers 400 "can't parse entities".

### 4. Configuration & lifecycle
- YAML config (`daily-brief.yaml`), `-config` flag (default
  `daily-brief.yaml`), `-verbose` for debug logging (logrus, same style as
  hostmonitor).
- Defaults + validation in `readConfig` (api key, telegram credentials,
  at least one source, per-source required fields, language `en`/`de`).
- **Prompts live in the config**: `prompts.system` / `prompts.user` are
  templates with `{date}`, `{language}` and `{sources}` placeholders
  (`renderPrompt` in `main.go`). The `{sources}` block is generated from the
  enabled sources (`buildSourcesDescription`) so it cannot drift. Empty
  prompts fall back to the defaults `defaultSystemPrompt` /
  `defaultUserPrompt` in `main.go`; the example config carries the same text
  (guarded by `TestExampleConfigPromptsMatchDefaults`). Tool descriptions stay
  in code — they belong to each tool's implementation.
- **Brief format**: the default prompt asks for emoji-rich formatting with
  `<b>` bold section headers (messages are sent with Telegram's HTML parse
  mode; the model must not use Markdown or stray `<`/`>`/`&`): 📰 Top news
  (sorted into 🏛️ Politics, 💶 Economy, ⚽ Sports, 🎭 Culture categories,
  deduplicated across sources — same story reported once), 📧 Emails,
  ☀️ PV system (⚡/🔋), 🚗 Car (🔋, with a note when the data is hours old),
  🐱 Cats (🏠/🌳, with the time the location last changed),
  🌤️ Weather (7-day monospace table inside `<pre>` tags, one line per day
  with weather emoji, temperature, precipitation and sunshine hours), 📺 TV
  news (copy-pasteable monospace `<pre>` block, one line per late-news
  edition: start time · channel · show, trailing ⏰ on the edition inside the
  configured window; a heute 19:00 re-watch tip line after the block when
  nothing is in the slot), 🔮 outlook.
- **Mistral Content type**: newer Mistral models return `content` as an array
  of content parts (`[{"type":"text","text":"..."}]`) instead of a plain
  string. The `Content` type in `mistral.go` is a string-based type with
  custom `UnmarshalJSON` that accepts both forms (concatenating text parts,
  ignoring non-text parts) and `MarshalJSON` that always emits the string
  form.
- Graceful shutdown via `signal.NotifyContext` (SIGINT/SIGTERM), plus an
  overall 15-minute run deadline (`overallRunTimeout`) and
  `TimeoutStartSec=10min` in the systemd unit, so a stuck LLM/tool/network
  call cannot block the timer indefinitely.
- systemd: `daily-brief.service` (`Type=oneshot`, dedicated `daily-brief`
  user, config at `/opt/daily-brief/daily-brief.yaml`) + `daily-brief.timer`
  (daily 05:00, `Persistent=true`). `make install` builds, creates the user
  (idempotent), installs units + example config, enables the timer.

## Go Dependencies
- `github.com/sirupsen/logrus` v1.9.4 — structured logging (direct)
- `gopkg.in/yaml.v2` v2.4.0 — YAML configuration (direct)
- `github.com/emersion/go-imap` v1.2.1 — IMAP client for Posteo (direct)
- `github.com/PuerkitoBio/goquery` v1.13.0 — HTML parsing/extraction (direct)
- `gitlab.com/bboehmke/sunny` v0.17.0 — SMA Speedwire protocol (direct; the
  same library evcc uses)
- `github.com/perbu/go-sure` v0.0.0-20251129095105-b73bcc0c9aab — Sure Petcare
  API client library, gosure (direct; BSD-3-Clause)
- `golang.org/x/net` — direct (charset handling via goquery; publicsuffix for
  the VW portal cookie jar)
- Mistral API, Telegram Bot API: plain `net/http`, no SDKs
- Module: `github.com/hseffler/daily-brief`

## Testing

- Tests live next to the code in `cmd/` and never touch the real network:
  - `ChatClient` interface → scripted fake LLM for agent-loop tests
    (parallel execution, message threading, max iterations, error paths).
  - `EmailFetcher` interface → fake fetcher for tool/state/dedupe tests.
  - `httptest.Server` for Mistral request shape, SMA Webconnect flow (login,
    getValues parsing, 401 re-login, error mapping), Telegram chunking/429,
    web-page extraction (HTML articles + RSS feeds), weather API parsing, the
    gosure→petStatus mapping (inside/outside, missing position/since), and
    the ARD/ZDF program APIs (response shapes, nested timeSlots, pagination,
    request headers, window/scope filtering, per-source failure handling).
  - `httptest` TLS servers fake the VW identity service and the EU Data Act
    portal (login redirect chain, session cookie, 401 re-login,
    marketing-consent skip, dataset zips, both vehicle-list response shapes).
    `go test -tags live -run TestLiveVWReadout ./cmd` does a manual readout
    against the real portal (reads `sources.ev` from the config).
    `go test -tags live -run TestLiveTVReadout ./cmd` does a manual readout
    against the real ARD/ZDF program APIs (prints the raw evening entries and
    the formatted `get_tv_news_schedule` output; needs no credentials).
    `go test -tags live -run TestLivePetsReadout ./cmd` does a manual readout
    against the real Sure Petcare API (reads `sources.pets` from the config;
    prints the raw pet locations and the formatted `get_pet_status` output).
- `Content` type is tested for both wire formats (string and array of parts),
  including a regression test for array content in Mistral responses.
- `emailSnippet` is tested against raw MIME fixtures (plain, multipart
  quoted-printable, base64).
- Run `gofmt -w`, `go vet ./...` and `go test ./...` (or `make test`) before
  committing. `go test -race ./...` is clean (the agent runs tools in
  goroutines).

## Maintenance Guidelines

When making changes to the codebase:
- **Always update the example configuration**: when configuration options
  change or are added/removed, update `daily-brief.yaml.example` to reflect
  the current state.
- **Always update documentation**: keep `README.md` and this document in sync
  with code changes.
- **Keep secrets out of code**: all URLs, API keys, tokens and passwords
  belong in the YAML config (gitignored `daily-brief.yaml`), never in source
  or tests.
- **Keep the seams**: new network-facing code must stay behind an injectable
  client/interface so tests use `httptest` or fakes. Never add a test that
  hits the real Mistral/Telegram/Posteo/SMA APIs.
- **Keep gofmt and tests green**: run `gofmt -w`, `go vet ./...`, and
  `go test ./...` (or `make test`) before committing.
- **SMA value changes**: the Speedwire register/OBIS mapping lives in the
  sunny library; `smaStatusFromValues` in `sma.go` maps `sunny.ValueID`s to
  `SMAStatus`. If you add values, extend that function and its fixtures in
  `sma_test.go`. The sunny library's own tests cover the wire protocol — do
  not add tests that talk to a real inverter.
- **Makefile install/uninstall require root**: `make install` creates the
  dedicated `daily-brief` system user if missing (idempotent), chowns
  `/opt/daily-brief` to it, and enables + starts the timer; `make uninstall`
  guards its `rm -rf` to `/opt/daily-brief` only.
- **Scheduling time**: change `OnCalendar` in `daily-brief.timer` (currently
  `*-*-* 05:00:00`).

## Known Limitations

1. **SMA coverage**: Speedwire only (via the sunny library): battery
   inverters (Sunny Boy Storage, Sunny Island, Tripower Smart Energy), plain
   Sunny Boy/Tripower inverters, SMA Energy Meter and Sunny Home Manager 2.0.
   ennexOS devices (Tripower X, Sunny Boy Smart Energy) speak a different
   protocol and are not covered. Discovery requires working multicast
   (239.12.255.254:9522) on the LAN — configure `sources.sma.host` to skip it.
2. **Bot-protected news sites**: zeit.de uses Centinel Analytica JavaScript
   bot protection on the main site (403 for non-browser clients). The RSS
   feed at `https://newsfeed.zeit.de/index` is accessible and used instead.
   spiegel.de exposes only teasers to anonymous fetches. taz and WELT are read
   via their RSS feeds.
3. **Single run per invocation**: designed for the systemd timer; there is
   no built-in scheduler loop.
4. **English/German briefs only**: `brief.language` is validated to `en`/`de`.
5. **Telegram HTML formatting**: messages are sent with `parse_mode=HTML`;
   the prompt asks the model for `<b>` section headers and `<pre>` blocks
   for the weather table, and no stray `<`/`>`/`&`. Telegram does not support
   `<table>` elements. If the model still emits markup Telegram rejects (400
   "can't parse entities"), the affected chunk is resent once as plain text
   with tags stripped, so delivery never fails on formatting.
6. **EV data freshness**: the EU Data Act portal delivers a dataset roughly
   every 15 minutes and only while the vehicle is active (driving or
   charging); a parked car's SoC/range may be hours old. The one-time browser
   setup (consent + link vehicle + enable the continuous data request) is
   mandatory. VW group brands only (VW, Audi, Skoda, Seat, Cupra).
7. **TV schedule scope**: `get_tv_news_schedule` checks the current day's own
   program (from 06:00 local in `sources.tv.timezone`); overnight repeats of
   the previous evening (before 06:00) are ignored, and the short late-night
   bulletin "heute journal update" is not matched as heute journal. The ZDF
   API is authenticated with the public token from ZDF's own web player
   (`sources.tv.zdf_api_key` overrides it if it ever rotates); the ARD
   program API serves at most 8 days ahead. Neither API is documented
   officially — both are the same endpoints the broadcasters' own web
   frontends use.
8. **Email is stateless**: mails are matched by a rolling `since_hours`
   window, not by dedupe state. A delayed run (systemd catch-up after
   downtime) or a manual second run repeats mails from the window, and
   mails from a failed run's window are not re-briefed.
9. **Pets (SureFlap)**: the Sure Petcare cloud API is unofficial and
   reverse-engineered — it may change or break without notice (a `position`
   outage in late 2024 was fixed by Sure Petcare, but there is no stability
   guarantee). The inside/outside state reflects flap passages and manual
   overrides only. Connect line only (Cat Flap Connect + Sure Petcare Hub);
   newer products (SureFlap EVO and friends) may use a different API. The
   gosure library decodes `position.since` as an RFC 3339 `time.Time` — a
   non-RFC 3339 timestamp (e.g. a zone-less one from a manual override set
   through an API client) would fail the whole pets request.

## Future Enhancements

1. **ennexOS support** for Tripower X / Sunny Boy Smart Energy.
2. **More output channels**: email delivery, markdown file archive.
3. **Web search tool** (e.g. DuckDuckGo) in addition to configured pages.
4. **Configurable brief sections/prompt** via YAML.

## Project Status

**Current**: Feature-complete for the requested sources, tests green
**Version**: 1.2.0

---

*This document is kept in sync with the code; update it whenever the
behavior, configuration, or file layout changes.*
