# Daily Brief

An agentic daily brief generator in Go. Every morning it gathers information
from your configured sources with an LLM agent (Mistral API) that uses tools,
writes a concise brief, and delivers it to your Telegram chat.

The project layout follows the conventions of
[hostmonitor](../hostmonitor): `cmd/` package with tests alongside, YAML
config with an `.example` file, logrus logging, a Makefile with
`GOOS`/`GOARCH` overrides, and a systemd service + timer for scheduling.

## What it does

1. An agent loop (Mistral `chat/completions` with tool calling) wakes up and
   reads the configured sources using four tools:
   - **`fetch_webpage`** — downloads a web page or RSS feed (e.g. zeit.de,
     spiegel.de, taz, welt) and returns its readable text (headlines/article
     teasers). RSS/XML feeds are detected automatically and rendered as a
     simple headline list.
   - **`get_new_emails`** — fetches mails from your Posteo mailbox over IMAP
     (TLS) that arrived within the configured `since_hours` window (default
     24h, matching the daily schedule). The mailbox is opened read-only (no
     `\Seen` flags are set). The tool is **stateless** — no state file: if
     more mails arrive than `max_messages`, only the newest are briefed, and
     a delayed or repeated run may report mails from the window again.
   - **`get_pv_status`** — reads your in-house SMA PV system via **SMA
     Speedwire (UDP)** — the same library evcc uses
     (`gitlab.com/bboehmke/sunny`), no external exporter: current PV
     production, battery state of charge, battery voltage/current/power,
     grid feed-in/draw, and today's/total yield.
   - **`get_weather_forecast`** — fetches a 7-day weather forecast from the
     free **Open-Meteo** API (no API key required): per-day condition,
     min/max temperature, precipitation amount and probability.
2. The model writes the final brief (English by default, configurable) with
   emoji-rich formatting: 📰 *Top news* (deduplicated across sources),
   📧 *Emails*, ☀️ *PV system*, 🌤️ *Weather* (7-day grid), and a 🔮 one-line
   outlook. Multiple news sources covering the same story are merged into a
   single bullet — no duplicate headlines.
3. The brief is printed to stdout and sent to your Telegram chat (chunked to
   fit the 4096-character message limit). Messages are sent with Telegram's
   HTML parse mode, so the brief's <b> section headers render bold; the rest
   of the formatting relies on emojis, Unicode symbols and spacing. If
   Telegram rejects the markup, the affected chunk is resent as plain text.

All URLs, credentials and API keys live in the YAML config — never in code.

## Quick start

```bash
# 1. Build
make build

# 2. Configure
cp daily-brief.yaml.example daily-brief.yaml
$EDITOR daily-brief.yaml

# 3. Run once
./build/daily-brief -config daily-brief.yaml

# 4. Run daily at 07:00 via systemd (requires root)
sudo make install
```

## Configuration

See [daily-brief.yaml.example](daily-brief.yaml.example) for all options with
comments. The important ones:

| Key | Purpose |
| --- | --- |
| `mistral.api_key` | Mistral API key (https://console.mistral.ai/) |
| `mistral.model` | e.g. `mistral-large-4`, `mistral-medium-latest`, `mistral-small-latest` |
| `brief.language` | `en` or `de` |
| `prompts.system` / `prompts.user` | prompt templates (see below) |
| `sources.web` | list of `{name, url}` news pages or RSS feeds |
| `sources.email` | Posteo IMAP: host `posteo.de`, port `993`, username = full address, password = **app password** |
| `sources.sma` | inverter host (empty = discover), password (default `0000`), serial, interface, timeout |
| `sources.weather` | Open-Meteo: `latitude`, `longitude`, `timezone`, `days` (default 7) |
| `telegram.token` / `telegram.chat_id` | Bot token from @BotFather; chat id via `getUpdates` |

### Posteo (email source)

Posteo requires an **app password** for mail clients: log in at posteo.de →
*Settings → "Password and security" → "App passwords"* and create one. The
username is your full Posteo address (`you@posteo.de`). The mailbox is opened
read-only (no `\Seen` flags are set).

### SMA PV system (Speedwire)

`get_pv_status` speaks **SMA Speedwire** (UDP, port 9522) natively — the same
approach and library as [evcc](https://github.com/evcc-io/evcc)
(`gitlab.com/bboehmke/sunny`). No external exporter (Speedwire daemon, Home
Assistant, ...) is needed.

- Point `sources.sma.host` at the device's IP or hostname on your LAN. With no
  `host`, devices are discovered via Speedwire multicast; `sources.sma.serial`
  selects one by serial number.
- `sources.sma.password` is the inverter's **user password** (max 12
  characters; SMA's factory default is `0000`, the same default evcc uses).
- Battery-capable inverters (**Sunny Boy Storage**, **Sunny Island**, **Sunny
  Tripower Smart Energy**) are read actively with that password; they report
  PV power, battery state of charge, battery voltage/current, grid power and
  energy counters.
- Plain **Sunny Boy/Tripower** inverters are read the same way, but report no
  battery and no grid exchange: their current AC output is shown as PV
  production, and the grid feed-in/draw line is n/a (an SMA Energy Meter
  would be needed for that).
- **SMA Energy Meter / Sunny Home Manager 2.0** broadcast their measurements
  via Speedwire multicast and need no password; they report grid power
  (import/export, per phase) and energy counters.
- The brief uses (via the sunny value IDs): `PvPower` (hybrids; plain
  inverters fall back to `ActivePowerPlus`, their current AC output),
  `BatteryCharge` (SOC), `BatteryVoltage` × `BatteryCurrent` (battery power,
  evcc's derivation), `BatteryTemperature`, `GridPowerImport`/
  `GridPowerExport` (inverters) or `ActivePowerPlus`/`ActivePowerMinus`
  (energy meters), `ActiveEnergyPlusTodayKWh` (yield today),
  `PvEnergyTotalKWh`, `BatteryEnergyChargeKWh`/`BatteryEnergyDischargeKWh`,
  `GridEnergyImportKWh`/`GridEnergyExportKWh`.

Note: ennexOS devices (Tripower X, Sunny Boy Smart Energy) speak a different
protocol and are not covered.

### Weather (7-day forecast)

`get_weather_forecast` uses the **Open-Meteo** forecast API — free and no API
key required. Configure your location:

```yaml
sources:
  weather:
    enabled: true
    latitude: 52.5200        # your location (example: Berlin)
    longitude: 13.4050
    timezone: "Europe/Berlin" # optional; the location's timezone when empty
    days: 7                  # forecast days (max 16)
```

The tool returns per day: weather condition (WMO code mapped to text),
min/max temperature, precipitation amount and probability. The brief renders
this as a compact 7-day grid, one line per day, with a weather emoji
(☀️ 🌤️ ⛅ ☁️ 🌧️ 🌦️ ❄️ ⛈️ 🌫️).

### RSS feeds as news sources

The `fetch_webpage` tool automatically detects RSS/Atom feeds (by the presence
of `<item>` elements) and extracts the headlines as a simple bullet list.
This is the recommended way to read news sites that block scrapers:

| Site | Main page | RSS feed (use this) |
| --- | --- | --- |
| DIE ZEIT | `https://www.zeit.de/` (bot-protected) | `https://newsfeed.zeit.de/index` |
| taz | `https://www.taz.de/` | `https://www.taz.de/rss.xml` |
| WELT | `https://www.welt.de/` | `https://www.welt.de/feeds/topnews.rss` |
| Der Spiegel | `https://www.spiegel.de/` | works directly |

## Customizing the prompts

The prompts sent to the LLM live in the config under `prompts:` — edit them
there, no code changes needed:

- `prompts.system` — the system prompt (role, rules, output format).
- `prompts.user` — the user prompt that kicks off the run.

Available placeholders, filled in by the program:

| Placeholder | Replaced with |
| --- | --- |
| `{date}` | today's date (`YYYY-MM-DD`) |
| `{language}` | the brief language (`English`/`German`) |
| `{sources}` | the generated list of the enabled sources and their tools |

When a prompt is empty, the built-in default is used (the example config
carries the same text). The `{sources}` block is generated from the enabled
sources so it cannot drift out of sync.

## Scheduling

The binary runs once per invocation (generate + deliver + exit), which makes
it a natural fit for a systemd timer:

- `daily-brief.service` — `Type=oneshot`, runs as the dedicated `daily-brief`
  user, config at `/opt/daily-brief/daily-brief.yaml`.
- `daily-brief.timer` — runs the service daily at 07:00 (`Persistent=true` to
  catch up after downtime). Change `OnCalendar` to adjust the time.

```bash
sudo make install                 # builds, installs binary + units, enables timer
sudo systemctl list-timers daily-brief.timer
sudo journalctl -u daily-brief.service -f   # watch the runs
sudo systemctl start daily-brief.service    # trigger a manual run
```

`make install` creates the `daily-brief` system user (idempotent), copies the
example config to `/opt/daily-brief/daily-brief.yaml` if none exists, and
chowns the install + config directories to the service user.

## Development

```bash
make test          # go test -v ./...
go test -race ./...# including the race detector
go vet ./...
gofmt -l cmd/      # should print nothing
./build/daily-brief -config daily-brief.yaml -verbose   # debug logging
```

The code is structured for testability the same way as hostmonitor: the LLM
client is behind the `ChatClient` interface, email fetching behind
`EmailFetcher`, and every HTTP client (Mistral, SMA, Telegram, web fetch) is
injectable, so tests run against `httptest` servers and never touch the real
network.

## Troubleshooting

- **`mistral.api_key is required`** — fill in `daily-brief.yaml`.
- **Telegram delivery fails** — check token/chat id; run with `-verbose`.
- **Email: login failed** — Posteo needs an *app password*, not your account
  password. Username must be the full address.
- **SMA: no device responding at `host`** — check the IP; with no `host`,
  discovery needs multicast (239.12.255.254:9522) to work on your LAN — some
  switches block it. Set `sources.sma.serial` to pick a discovered device.
- **SMA: login failed** — check `sources.sma.password` (the inverter's *user*
  password, default `0000`, max 12 characters).
- **SMA: energy meter does not respond** — the Energy Meter / Home Manager
  only sends broadcasts; make sure nothing blocks UDP port 9522 between the
  device and this host.
- **zeit.de returns 403** — the main site uses JavaScript bot protection
  (Centinel Analytica). Use the RSS feed at `https://newsfeed.zeit.de/index`
  instead (the default in the example config).
- **Paywalled news** — spiegel.de only exposes teasers to anonymous fetches;
  the brief summarizes what is publicly visible.
