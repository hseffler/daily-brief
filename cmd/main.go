// Command daily-brief generates a personalized daily brief with an agentic
// LLM loop (Mistral API) and delivers it via Telegram.
//
// Sources (all configured in the YAML config, never hard-coded):
//   - web pages (e.g. zeit.de, spiegel.de) via the fetch_webpage tool
//   - a Posteo mailbox (new mails) via the get_new_emails tool (IMAP)
//   - an in-house SMA PV system (battery state, current production) via the
//     get_pv_status tool (SMA Speedwire, via the sunny library — evcc's approach)
//   - a VW group electric vehicle (battery state of charge, range, charging
//     status) via the get_ev_status tool (VW EU Data Act portal — evcc's
//     "drivesomethinggreater" approach)
//   - the evening TV program of ARD (Das Erste) and ZDF via the
//     get_tv_news_schedule tool (public program APIs — do heute-journal and
//     tagesthemen start early, within the configured window?)
//   - the user's cats (inside/outside) via the get_pet_status tool (Sure
//     Petcare cloud API behind the SureFlap cat flap)
//
// The binary runs once per invocation and is meant to be scheduled daily
// (systemd timer, see daily-brief.timer).
package main

import (
	"context"
	"flag"
	"fmt"
	"html"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
)

func main() {
	configFile := flag.String("config", "daily-brief.yaml", "Path to YAML config file")
	verbose := flag.Bool("verbose", false, "Enable verbose debug logging")
	flag.Parse()

	logger := logrus.New()
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
		DisableColors:   true,
	})
	logger.SetLevel(logrus.InfoLevel)
	logger.SetOutput(os.Stdout)
	if *verbose {
		logger.SetLevel(logrus.DebugLevel)
		logger.Info("🐛 Verbose debug logging enabled")
	}

	cfg, err := readConfig(*configFile)
	if err != nil {
		logger.Fatalf("Failed to load config: %v", err)
	}

	logger.WithFields(logrus.Fields{
		"model":       cfg.Mistral.Model,
		"language":    cfg.Brief.Language,
		"web_sources": len(cfg.Sources.Web),
		"email":       cfg.Sources.Email.Enabled,
		"sma":         cfg.Sources.SMA.Enabled,
		"weather":     cfg.Sources.Weather.Enabled,
		"ev":          cfg.Sources.EV.Enabled,
		"tv":          cfg.Sources.TV.Enabled,
		"pets":        cfg.Sources.Pets.Enabled,
		"max_iter":    cfg.Mistral.MaxIterations,
	}).Info("🚀 Starting daily brief generation")

	// Cancel in-flight work on SIGINT/SIGTERM so a hung tool or LLM call
	// winds down instead of being SIGKILLed.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Bound the whole run (gather + write + deliver) so a stuck LLM/tool/
	// network call cannot block the systemd timer indefinitely. The systemd
	// unit additionally has TimeoutStartSec.
	runCtx, cancel := context.WithTimeout(ctx, overallRunTimeout)
	defer cancel()

	brief, err := generateBrief(runCtx, cfg, logger)
	if err != nil {
		logger.Errorf("Failed to generate brief: %v", err)
		// A failed run must not be silent: notify via Telegram (with a fresh
		// short deadline — runCtx may already be expired) before exiting.
		sender := NewTelegramSender(cfg.Telegram.Token, cfg.Telegram.ChatID, nil, logger)
		noticeCtx, cancelNotice := context.WithTimeout(ctx, 60*time.Second)
		defer cancelNotice()
		notice := "⚠️ <b>Daily brief failed</b>\n" + html.EscapeString(fmt.Sprintf("brief generation failed: %v", err))
		if sendErr := sender.Send(noticeCtx, notice); sendErr != nil {
			logger.Errorf("Failed to send the failure notice via Telegram: %v", sendErr)
		}
		logger.Fatalf("Failed to generate brief: %v", err)
	}

	fmt.Printf("\n========== Daily Brief %s ==========\n\n%s\n\n", time.Now().Format("2006-01-02"), brief)

	sender := NewTelegramSender(cfg.Telegram.Token, cfg.Telegram.ChatID, nil, logger)
	if err := sender.Send(runCtx, brief); err != nil {
		logger.Fatalf("Failed to deliver brief via Telegram: %v", err)
	}
}

// overallRunTimeout bounds one whole run (gather + write + deliver).
const overallRunTimeout = 15 * time.Minute

// generateBrief wires the tools and the agent and returns the final brief.
func generateBrief(ctx context.Context, cfg *Config, logger *logrus.Logger) (string, error) {
	mistral := NewMistralClient(cfg.Mistral.APIKey, cfg.Mistral.BaseURL, nil, logger)

	tools := []Tool{
		newWebFetchTool(cfg.WebFetch.MaxChars, cfg.WebFetch.TimeoutSeconds, logger),
	}

	if cfg.Sources.Email.Enabled {
		fetcher := &imapFetcher{
			host:     cfg.Sources.Email.Host,
			port:     cfg.Sources.Email.Port,
			username: cfg.Sources.Email.Username,
			password: cfg.Sources.Email.Password,
			mailbox:  cfg.Sources.Email.Mailbox,
			logger:   logger,
		}
		tools = append(tools, newEmailTool(cfg, fetcher, logger))
	}

	if cfg.Sources.SMA.Enabled {
		smaClient, err := NewSMASpeedwireClient(
			cfg.Sources.SMA.Host,
			cfg.Sources.SMA.Password,
			cfg.Sources.SMA.Serial,
			cfg.Sources.SMA.Interface,
			cfg.Sources.SMA.TimeoutSeconds,
			logger,
		)
		if err != nil {
			return "", err
		}
		if cfg.Sources.SMA.Host == "" && cfg.Sources.SMA.Serial == 0 {
			logger.Warn("sources.sma: no host/serial configured — the first SMA device found on the LAN will be used")
		}
		tools = append(tools, newSMATool(smaClient, logger))
	}

	if cfg.Sources.Weather.Enabled {
		weatherClient := newWeatherClient(
			cfg.Sources.Weather.Latitude,
			cfg.Sources.Weather.Longitude,
			cfg.Sources.Weather.Timezone,
			cfg.Sources.Weather.Days,
			cfg.Sources.Weather.TimeoutSeconds,
			cfg.Sources.Weather.URL,
			logger,
		)
		tools = append(tools, newWeatherTool(weatherClient, logger))
	}

	if cfg.Sources.EV.Enabled {
		evClient, err := newVWPortalClient(
			cfg.Sources.EV.Brand,
			cfg.Sources.EV.User,
			cfg.Sources.EV.Password,
			cfg.Sources.EV.VIN,
			cfg.Sources.EV.PortalURL,
			cfg.Sources.EV.IdentityURL,
			cfg.Sources.EV.TimeoutSeconds,
			nil,
			logger,
		)
		if err != nil {
			return "", err
		}
		tools = append(tools, newEVTool(evClient, logger))
	}

	if cfg.Sources.TV.Enabled {
		tvClient, err := newTVClient(
			cfg.Sources.TV.WindowStart,
			cfg.Sources.TV.WindowEnd,
			cfg.Sources.TV.Timezone,
			cfg.Sources.TV.TimeoutSeconds,
			cfg.Sources.TV.ARDURL,
			cfg.Sources.TV.ZDFURL,
			cfg.Sources.TV.ZDFAPIKey,
			logger,
		)
		if err != nil {
			return "", err
		}
		tools = append(tools, newTVTool(tvClient, logger))
	}

	if cfg.Sources.Pets.Enabled {
		petsClient := newSureFlapClient(cfg.Sources.Pets.Email, cfg.Sources.Pets.Password)
		tools = append(tools, newPetsTool(petsClient, logger))
	}

	now := time.Now()
	systemPrompt := buildSystemPrompt(cfg, now)
	userPrompt := buildUserPrompt(cfg, now)

	// cfg comes from readConfig, which runs setDefaults: Temperature is set.
	agent := NewAgent(mistral, cfg.Mistral.Model, *cfg.Mistral.Temperature,
		tools, systemPrompt, userPrompt, cfg.Mistral.MaxIterations, logger)

	brief, err := agent.Run(ctx)
	return brief, err
}

// defaultSystemPrompt and defaultUserPrompt are the fallback prompt templates,
// used when the config does not set prompts.system / prompts.user. The
// example config carries the same text so it can be edited there.
const defaultSystemPrompt = `You are the user's personal assistant. Your job is to write a concise, visually appealing daily brief.
Today's date: {date}.

{sources}

Rules:
- Write the brief in {language}.
- Use emojis and visual formatting to make the brief scannable and pleasant to read. The message is sent to Telegram with HTML formatting: make each section header bold with <b>...</b> (e.g. 📰 <b>Top news</b>) and keep the emojis. For the weather section, wrap the forecast in <pre>...</pre> to create a monospace table layout. Use no other markup — no Markdown (**bold**, *italic*, backticks), no HTML tables (Telegram does not support <table>), it would be shown literally. Outside <b> and <pre> tags, never write the characters <, > or & (write "and" instead of "&").
- Structure the brief with exactly these sections, in this order, each with an emoji-prefixed header and a blank line before it:
  1. 📰 "Top news": sort the most important headlines into categories, each with an emoji-prefixed bold header. Use these categories (skip any that have no stories): 🏛️ <b>Politics</b>, 💶 <b>Economy</b>, ⚽ <b>Sports</b>, 🎭 <b>Culture</b>. Under each category, 1-2 bullet points (use ▪ or • as the bullet), one line each. Multiple sources often cover the same story — report each event only once, merging details from different sources into a single bullet. Never list the same news item twice. Total: 3-5 bullet points across all categories.
  2. 📧 "Emails": only noteworthy mails (skip newsletters, spam and automated notifications), one line each: sender — subject.
  3. ☀️ "PV system": current PV production, battery state of charge, and a one-line assessment (e.g. battery charging from solar, surplus feeding into the grid). Use ⚡ for production, 🔋 for battery.
  4. 🚗 "Car": the user's electric vehicle — battery state of charge, estimated range and charging status. Use 🔋 for the state of charge. When the data is more than a few hours old, mention that.
  5. 🐱 "Cats": for each cat, whether it is inside (🏠) or outside (🌳), and since when the location last changed. One line per cat.
  6. 🌤️ "Weather": a compact 7-day forecast in a monospace table inside <pre>...</pre> tags, one line per day. Columns: weather emoji, short day name and date, temperature range, precipitation, sunshine hours. Use · as a separator between columns. Examples:
     <pre>☀️ Mon 09 ·  8–17°C · 0.0mm · 5.2h sun
     🌧️ Tue 10 ·  9–15°C · 2.3mm (80%)
     ⛅ Wed 11 · 10–16°C · 0.5mm (20%) · 3.1h sun</pre>
     Weather emojis: ☀️ clear, 🌤️ mostly sunny, ⛅ partly cloudy, ☁️ overcast, 🌧️ rain, 🌦️ showers, ❄️ snow, ⛈️ thunderstorm, 🌫️ fog. Only show the precipitation probability when it is above 0%. Only show sunshine hours when above 0.
  7. 📺 "TV news": the coming day's late news editions from the get_tv_news_schedule tool, as a compact monospace block inside <pre>...</pre> tags — ready to copy and paste, like the weather table. One line per edition. Use · as a separator between columns: start–end time (HH:MM–HH:MM), channel (ZDF / ARD), show name (heute journal / tagesthemen). Mark an edition that starts within the configured window with a trailing ⏰. Omit editions the tool reports as not scheduled that day. If no edition starts within the window, write one line after the block suggesting to re-watch heute (19:00) in the ZDF Mediathek. Example:
     <pre>21:30–22:00 · ZDF · heute journal ⏰
     23:30–00:00 · ARD · tagesthemen</pre>
  8. 🔮 End with a single-sentence outlook for the day, based only on the weather forecast.
- Separate sections with a blank line. You may use a thin separator line (e.g. ───) between major sections if it improves readability.
- Keep the whole brief under 300 words. Be concise and factual.
- Only report information the tools actually returned. Never invent news, emails or measurements.
- When you have gathered enough information, reply with the final brief as your message and make no further tool calls.`

const defaultUserPrompt = `Generate today's daily brief (date: {date}). Use your tools to gather the information first, then write the final brief as your reply.`

// buildSystemPrompt renders the system prompt: the template from the config
// (or the default) with the dynamic placeholders filled in.
func buildSystemPrompt(cfg *Config, now time.Time) string {
	tpl := cfg.Prompts.System
	if strings.TrimSpace(tpl) == "" {
		tpl = defaultSystemPrompt
	}
	return renderPrompt(tpl, cfg, now)
}

// buildUserPrompt renders the user prompt the same way.
func buildUserPrompt(cfg *Config, now time.Time) string {
	tpl := cfg.Prompts.User
	if strings.TrimSpace(tpl) == "" {
		tpl = defaultUserPrompt
	}
	return renderPrompt(tpl, cfg, now)
}

// renderPrompt replaces the dynamic placeholders in a prompt template:
//
//	{date}     - today's date (YYYY-MM-DD)
//	{language} - the brief language (English/German)
//	{sources}  - the generated list of enabled sources and their tools
func renderPrompt(tpl string, cfg *Config, now time.Time) string {
	return strings.NewReplacer(
		"{date}", now.Format("2006-01-02"),
		"{language}", languageName(cfg.Brief.Language),
		"{sources}", buildSourcesDescription(cfg),
	).Replace(tpl)
}

// buildSourcesDescription lists the enabled sources and the tools to use for
// them. It is generated from the config so it cannot drift from the enabled
// sources.
func buildSourcesDescription(cfg *Config) string {
	var sb strings.Builder
	sb.WriteString("Available sources (use the tools to gather data before writing):")
	for _, w := range cfg.Sources.Web {
		fmt.Fprintf(&sb, "\n- Web page %q: %s — fetch it with the fetch_webpage tool and extract the most important headlines.", w.Name, w.URL)
	}
	if cfg.Sources.Email.Enabled {
		sb.WriteString("\n- Email: the user's Posteo mailbox — call get_new_emails to see the mails that arrived within the configured time window (sources.email.since_hours).")
	}
	if cfg.Sources.SMA.Enabled {
		sb.WriteString("\n- SMA PV system: call get_pv_status to read the battery state of charge and the current production.")
	}
	if cfg.Sources.Weather.Enabled {
		fmt.Fprintf(&sb, "\n- Weather: call get_weather_forecast for the %d-day forecast at the user's location.", cfg.Sources.Weather.Days)
	}
	if cfg.Sources.EV.Enabled {
		sb.WriteString("\n- Electric vehicle (VW group, via the EU Data Act portal): call get_ev_status to read the battery state of charge, estimated range and charging status of the user's car.")
	}
	if cfg.Sources.TV.Enabled {
		ws, we := tvWindowBounds(cfg.Sources.TV.WindowStart, cfg.Sources.TV.WindowEnd)
		fmt.Fprintf(&sb, "\n- TV news (ARD Das Erste + ZDF): call get_tv_news_schedule to check the coming day's program — whether the late news editions heute-journal (ZDF) or tagesthemen (Das Erste) start within the %s window.", tvWindowLabel(ws, we))
	}
	if cfg.Sources.Pets.Enabled {
		sb.WriteString("\n- Cats (SureFlap cat flap, via the Sure Petcare cloud API): call get_pet_status to see for each cat whether it is inside or outside, and since when the location last changed.")
	}
	return sb.String()
}

// languageName maps the config language code to the name used in the prompt.
func languageName(code string) string {
	switch code {
	case "de":
		return "German"
	case "en":
		return "English"
	default:
		return code
	}
}
