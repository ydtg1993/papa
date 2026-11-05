package discovery

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

type XMLTarget struct {
	URL     string
	Sitemap bool
}
type XMLTargets struct {
	Targets   []XMLTarget
	Truncated bool
}

// ParseXML discovers targets from sitemap urlset/index, RSS 2 and Atom.
// It never follows sitemap indexes or fetches external XML entities. Input is
// limited to 8 MiB; gzip must be decoded by the caller under the same limit.
func ParseXML(data []byte, base string, maxTargets int) (XMLTargets, error) {
	var out XMLTargets
	if len(data) > 8<<20 {
		return out, fmt.Errorf("discovery XML exceeds 8 MiB")
	}
	if maxTargets <= 0 {
		maxTargets = 10000
	}
	var doc struct {
		URLs []struct {
			Loc string `xml:"loc"`
		} `xml:"url"`
		Sitemaps []struct {
			Loc string `xml:"loc"`
		} `xml:"sitemap"`
		Channel struct {
			Items []struct {
				Link string `xml:"link"`
			} `xml:"item"`
		} `xml:"channel"`
		Entries []struct {
			Links []struct {
				Href string `xml:"href,attr"`
				Rel  string `xml:"rel,attr"`
			} `xml:"link"`
		} `xml:"entry"`
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	// Read the root ourselves to reject unknown formats and trailing documents.
	var root xml.StartElement
	for {
		tok, err := decoder.Token()
		if err != nil {
			return out, err
		}
		if start, ok := tok.(xml.StartElement); ok {
			root = start
			break
		}
		if text, ok := tok.(xml.CharData); ok && len(bytes.TrimSpace(text)) > 0 {
			return out, fmt.Errorf("text before XML root")
		}
	}
	if err := decoder.DecodeElement(&doc, &root); err != nil {
		return out, err
	}
	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, err
		}
		if _, ok := tok.(xml.StartElement); ok {
			return out, fmt.Errorf("multiple XML roots")
		}
		if text, ok := tok.(xml.CharData); ok && len(bytes.TrimSpace(text)) > 0 {
			return out, fmt.Errorf("trailing XML text")
		}
	}
	seen := map[string]bool{}
	add := func(ref string, index bool) {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return
		}
		u, err := CanonicalURL(base, ref)
		if err != nil || seen[u] {
			return
		}
		seen[u] = true
		if len(out.Targets) >= maxTargets {
			out.Truncated = true
			return
		}
		out.Targets = append(out.Targets, XMLTarget{URL: u, Sitemap: index})
	}
	switch root.Name.Local {
	case "urlset":
		for _, r := range doc.URLs {
			add(r.Loc, false)
		}
	case "sitemapindex":
		for _, r := range doc.Sitemaps {
			add(r.Loc, true)
		}
	case "rss":
		for _, r := range doc.Channel.Items {
			add(r.Link, false)
		}
	case "feed":
		for _, r := range doc.Entries {
			for _, l := range r.Links {
				if l.Rel == "" || l.Rel == "alternate" {
					add(l.Href, false)
				}
			}
		}
	default:
		return out, fmt.Errorf("unsupported XML feed %q", root.Name.Local)
	}
	return out, nil
}
