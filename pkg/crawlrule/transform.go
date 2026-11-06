package crawlrule

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v3/pkg/discovery"
)

// Transform is one deterministic scalar conversion, applied in listed order.
// Missing and null values bypass transforms and retain their validation meaning.
// Supported ops:
//   - trim: trim a string's surrounding Unicode whitespace.
//   - absolute_url: resolve a nonblank string against the final response URL;
//     only canonical HTTP(S) URLs without userinfo are accepted.
//   - number: accept a string or json.Number in strict JSON number syntax and
//     finite float64 range, retaining the exact spelling as json.Number.
//   - bool: accept a bool or the exact strings "true" and "false".
//   - date: parse a string using Layout (a Go time layout with a complete date),
//     assume UTC when no offset is supplied, and emit UTC RFC3339Nano.
//
// Only date accepts Layout. Named timezone abbreviations are not supported;
// use numeric offsets to avoid host-dependent timezone interpretation. String
// conversions do not trim implicitly, except absolute_url's URL canonicalizer;
// HTML extraction already trims text and attributes before all transforms.
type Transform struct {
	Op     string `json:"op"`
	Layout string `json:"layout,omitempty"`
}

var strictNumber = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

func compileTransforms(transforms []Transform) (string, error) {
	if len(transforms) > MaxTransformsLimit {
		return "", fmt.Errorf("at most %d transforms per field are supported", MaxTransformsLimit)
	}
	var kind string
	for i, transform := range transforms {
		if transform.Op != "date" && transform.Layout != "" {
			return "", fmt.Errorf("transform %d: layout is only supported by date", i)
		}
		switch transform.Op {
		case "trim":
			kind = "string"
		case "absolute_url":
			kind = "http-url"
		case "number":
			kind = "number"
		case "bool":
			kind = "bool"
		case "date":
			if err := validateDateLayout(transform.Layout); err != nil {
				return "", fmt.Errorf("transform %d: %w", i, err)
			}
			kind = "string"
		default:
			return "", fmt.Errorf("transform %d: unsupported op %q", i, transform.Op)
		}
	}
	return kind, nil
}

func validateDateLayout(layout string) error {
	if layout == "" || len(layout) > 256 || strings.Contains(layout, "MST") {
		return fmt.Errorf("date requires a Go layout of 1..256 bytes without named timezone abbreviations")
	}
	// Go layouts may consist entirely of literals. A reference round trip
	// rejects such layouts and those that omit a complete calendar date.
	reference := time.Date(2024, time.November, 23, 17, 45, 56, 123456789, time.UTC)
	parsed, err := time.ParseInLocation(layout, reference.Format(layout), time.UTC)
	if err != nil || parsed.Year() != reference.Year() || parsed.Month() != reference.Month() || parsed.Day() != reference.Day() {
		return fmt.Errorf("date layout must describe a complete calendar date")
	}
	return nil
}

func transformValue(value any, transform Transform, base string) (any, error) {
	switch transform.Op {
	case "trim":
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("expected string")
		}
		return strings.TrimSpace(text), nil
	case "absolute_url":
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("expected nonblank URL string")
		}
		canonical, err := discovery.CanonicalURL(base, text)
		if err != nil {
			return nil, fmt.Errorf("expected HTTP(S) URL without userinfo")
		}
		return canonical, nil
	case "number":
		var text string
		switch v := value.(type) {
		case string:
			text = v
		case json.Number:
			text = v.String()
		default:
			return nil, fmt.Errorf("expected string or JSON number")
		}
		if !strictNumber.MatchString(text) {
			return nil, fmt.Errorf("expected strict JSON number syntax")
		}
		number, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, fmt.Errorf("number exceeds finite float64 range")
		}
		return json.Number(text), nil
	case "bool":
		switch v := value.(type) {
		case bool:
			return v, nil
		case string:
			if v == "true" {
				return true, nil
			}
			if v == "false" {
				return false, nil
			}
		}
		return nil, fmt.Errorf("expected bool or exact true/false string")
	case "date":
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("expected date string")
		}
		parsed, err := time.ParseInLocation(transform.Layout, text, time.UTC)
		if err != nil {
			return nil, fmt.Errorf("date does not match configured layout")
		}
		utc := parsed.UTC()
		// A valid four-digit input year can cross the RFC3339 boundaries when
		// its numeric timezone offset is normalized to UTC. Format alone does
		// not reject negative years or years with more than four digits.
		if utc.Year() < 0 || utc.Year() > 9999 {
			return nil, fmt.Errorf("normalized date exceeds RFC3339 year range")
		}
		return utc.Format(time.RFC3339Nano), nil
	default:
		return nil, fmt.Errorf("unsupported transform")
	}
}
