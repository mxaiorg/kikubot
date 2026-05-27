package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kikubot/internal/config"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// ── WeatherAPI.com Tool ─────────────────────────────────────────────────
//
// Single tool over the WeatherAPI.com REST API
// (https://app.swaggerhub.com/apis-docs/WeatherAPI.com/WeatherAPI/1.0.2).
//
// The model picks an `endpoint` (current | forecast | history | future |
// search | marine | astronomy | timezone | ip | alerts) and supplies a
// location `q` plus any endpoint-specific extras. All responses are
// JSON.
//
// Auth: WEATHERAPI_KEY sent as the `key` query parameter (per the API).

const weatherBaseURL = "https://api.weatherapi.com/v1"

func Weather() []ToolDefinition {
	if strings.TrimSpace(config.WeatherApiKey) == "" {
		log.Println("[weather] WEATHERAPI_KEY not set — Weather tool disabled")
		return nil
	}
	log.Println("[weather] REST client initialized")
	return []ToolDefinition{weatherTool()}
}

func weatherTool() ToolDefinition {
	return ToolDefinition{
		Name: "weather",
		Description: "Weather data via WeatherAPI.com. Pick an `endpoint` and supply `q` " +
			"(city, \"lat,lon\", postcode, IP, airport code, or location id). Endpoints: " +
			"current (real-time), forecast (1-14 days), history (since 2010-01-01), future " +
			"(14-300 days out), search (location autocomplete), marine (tides & marine " +
			"conditions), astronomy (sun/moon for a date), timezone, ip (geo-locate an IP " +
			"— pass it as `q`), alerts (active weather alerts). Returns raw JSON.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"endpoint": {
					"type": "string",
					"enum": ["current","forecast","history","future","search","marine","astronomy","timezone","ip","alerts"],
					"description": "Which WeatherAPI endpoint to call."
				},
				"q": {
					"type": "string",
					"description": "Location query. City name, \"lat,lon\", US/UK/Canada postcode, IP (\"auto:ip\" for caller), airport code (e.g. iata:DXB), or weatherapi location id. For the ip endpoint, pass the IP address."
				},
				"days": {
					"type": "integer",
					"minimum": 1,
					"maximum": 14,
					"description": "forecast only: number of forecast days (1-14)."
				},
				"dt": {
					"type": "string",
					"description": "history/future/astronomy/marine: date in yyyy-MM-dd. history >= 2010-01-01; future 14-300 days from today."
				},
				"end_dt": {
					"type": "string",
					"description": "history only: optional end date (yyyy-MM-dd) for a date range."
				},
				"hour": {
					"type": "integer",
					"minimum": 0,
					"maximum": 23,
					"description": "current/forecast/history: limit to a single hour (0-23)."
				},
				"aqi": {
					"type": "string",
					"enum": ["yes","no"],
					"description": "current/forecast/history: include air quality data. Default no."
				},
				"alerts": {
					"type": "string",
					"enum": ["yes","no"],
					"description": "forecast only: include weather alerts. Default no."
				},
				"lang": {
					"type": "string",
					"description": "Optional language code for condition text (e.g. en, es, fr, de, pt, it, zh)."
				},
				"tides": {
					"type": "string",
					"enum": ["yes","no"],
					"description": "marine only: include tide data. Default no."
				}
			},
			"required": ["endpoint","q"]
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				Endpoint string `json:"endpoint"`
				Q        string `json:"q"`
				Days     int    `json:"days,omitempty"`
				Dt       string `json:"dt,omitempty"`
				EndDt    string `json:"end_dt,omitempty"`
				Hour     *int   `json:"hour,omitempty"`
				Aqi      string `json:"aqi,omitempty"`
				Alerts   string `json:"alerts,omitempty"`
				Lang     string `json:"lang,omitempty"`
				Tides    string `json:"tides,omitempty"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}
			endpoint := strings.ToLower(strings.TrimSpace(p.Endpoint))
			if endpoint == "" {
				return "", fmt.Errorf("endpoint is required")
			}
			if strings.TrimSpace(p.Q) == "" {
				return "", fmt.Errorf("q is required")
			}

			path, ok := weatherPaths[endpoint]
			if !ok {
				return "", fmt.Errorf("unknown endpoint %q", p.Endpoint)
			}

			q := url.Values{}
			q.Set("key", config.WeatherApiKey)
			q.Set("q", p.Q)
			if p.Lang != "" {
				q.Set("lang", p.Lang)
			}

			switch endpoint {
			case "forecast":
				if p.Days > 0 {
					q.Set("days", fmt.Sprintf("%d", p.Days))
				}
				if p.Alerts != "" {
					q.Set("alerts", p.Alerts)
				}
				if p.Aqi != "" {
					q.Set("aqi", p.Aqi)
				}
				if p.Hour != nil {
					q.Set("hour", fmt.Sprintf("%d", *p.Hour))
				}
				if p.Dt != "" {
					q.Set("dt", p.Dt)
				}
			case "current":
				if p.Aqi != "" {
					q.Set("aqi", p.Aqi)
				}
			case "history":
				if p.Dt == "" {
					return "", fmt.Errorf("dt (yyyy-MM-dd) is required for history")
				}
				q.Set("dt", p.Dt)
				if p.EndDt != "" {
					q.Set("end_dt", p.EndDt)
				}
				if p.Hour != nil {
					q.Set("hour", fmt.Sprintf("%d", *p.Hour))
				}
				if p.Aqi != "" {
					q.Set("aqi", p.Aqi)
				}
			case "future":
				if p.Dt == "" {
					return "", fmt.Errorf("dt (yyyy-MM-dd, 14-300 days out) is required for future")
				}
				q.Set("dt", p.Dt)
			case "astronomy":
				if p.Dt != "" {
					q.Set("dt", p.Dt)
				}
			case "marine":
				if p.Dt != "" {
					q.Set("dt", p.Dt)
				}
				if p.Tides != "" {
					q.Set("tides", p.Tides)
				}
			case "search", "timezone", "ip", "alerts":
				// only key + q (+ lang)
			}

			reqURL := fmt.Sprintf("%s%s?%s", weatherBaseURL, path, q.Encode())
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
			if err != nil {
				return "", fmt.Errorf("creating request: %w", err)
			}
			req.Header.Set("Accept", "application/json")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return "", fmt.Errorf("weather request failed: %w", err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return "", fmt.Errorf("reading response: %w", err)
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return "", fmt.Errorf("weather API error (HTTP %d): %s", resp.StatusCode, string(body))
			}
			return string(body), nil
		},
		StaticSystem: "- Weather data comes from WeatherAPI.com. Always supply a concrete `q` (city, \"lat,lon\", postcode, airport code, or \"auto:ip\"). For history use yyyy-MM-dd dates from 2010-01-01 onward; future dates must be 14-300 days from today.\n",
	}
}

var weatherPaths = map[string]string{
	"current":   "/current.json",
	"forecast":  "/forecast.json",
	"history":   "/history.json",
	"future":    "/future.json",
	"search":    "/search.json",
	"marine":    "/marine.json",
	"astronomy": "/astronomy.json",
	"timezone":  "/timezone.json",
	"ip":        "/ip.json",
	"alerts":    "/alerts.json",
}
