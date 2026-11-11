package server

import (
	"encoding/json"
	"errors"
	"math/big"
	"strconv"
	"strings"
)

// ImagePricing is USD per image or per million tokens. Decimal strings keep
// explicit zero distinct from missing and avoid binary rounding during billing.
type ImagePricing struct {
	Variants    []ImagePriceVariant `json:"variants,omitempty"`
	TextInput   *string             `json:"text_input,omitempty"`
	ImageInput  *string             `json:"image_input,omitempty"`
	TextOutput  *string             `json:"text_output,omitempty"`
	ImageOutput *string             `json:"image_output,omitempty"`
	// Only enable for upstreams whose usage.cost contract is documented as USD.
	ProviderCost          bool `json:"provider_cost,omitempty"`
	UnsupportedConditions bool `json:"unsupported_conditions,omitempty"`
}

type ImagePriceVariant struct {
	Size    string `json:"size"`
	Quality string `json:"quality"`
	Price   string `json:"price"`
}

type MediaPriceSnapshot struct {
	Input       string        `json:"input,omitempty"`
	Output      string        `json:"output,omitempty"`
	Cache       string        `json:"cache,omitempty"`
	Image       *float64      `json:"image,omitempty"`
	ImagePrices *ImagePricing `json:"image_prices,omitempty"`
	Size        string        `json:"size,omitempty"`
	Quality     string        `json:"quality,omitempty"`
}

func decimalRate(s string) (*big.Rat, bool) {
	if len(s) == 0 || len(s) > 64 || !json.Valid([]byte(s)) || s[0] == '-' {
		return nil, false
	}
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exp, err := strconv.Atoi(s[i+1:])
		if err != nil || exp < -308 || exp > 308 {
			return nil, false
		}
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 || r.Cmp(big.NewRat(1_000_000_000, 1)) > 0 {
		return nil, false
	}
	return r, true
}

func validateImagePricing(p *ImagePricing) error {
	if p == nil {
		return nil
	}
	if len(p.Variants) > 100 {
		return errors.New("at most 100 image price variants are allowed")
	}
	seen := map[string]bool{}
	for _, v := range p.Variants {
		key := v.Size + "\x00" + v.Quality
		if _, ok := decimalRate(v.Price); !ok || len(v.Size) > 32 || len(v.Quality) > 32 || seen[key] || (v.Size == "" && v.Quality == "") {
			return errors.New("image price variants require a size or quality condition, unique pairs, and non-negative decimal USD rates")
		}
		seen[key] = true
	}
	for _, rate := range []*string{p.TextInput, p.ImageInput, p.TextOutput, p.ImageOutput} {
		if rate != nil {
			if _, ok := decimalRate(*rate); !ok {
				return errors.New("image token rates must be non-negative decimal USD per million tokens")
			}
		}
	}
	return nil
}

func priceSnapshot(price requestPrice, image *float64, p *ImagePricing) *MediaPriceSnapshot {
	s := &MediaPriceSnapshot{Image: image, ImagePrices: p}
	if price.known {
		s.Input = strconv.FormatFloat(price.input, 'f', -1, 64)
		s.Output = strconv.FormatFloat(price.output, 'f', -1, 64)
		s.Cache = strconv.FormatFloat(price.cache, 'f', -1, 64)
	}
	return s
}

func (s *MediaPriceSnapshot) cost(u mediaResponse, req mediaRequest, operation string) (float64, bool, string) {
	if s.ImagePrices != nil && s.ImagePrices.ProviderCost && u.Cost != "" {
		if r, ok := decimalRate(u.Cost); ok {
			f, _ := r.Float64()
			return f, true, "provider_reported"
		}
	}
	if operation == "images" && s.ImagePrices != nil && s.ImagePrices.UnsupportedConditions {
		return 0, false, "unknown"
	}
	if operation == "images" && u.ImagesKnown {
		var rate *big.Rat
		if s.ImagePrices != nil && len(s.ImagePrices.Variants) > 0 {
			// A variant only constrains the fields it names; unspecified fields
			// (for example a size-only catalog row) match any request value.
			// Never guess a condition the catalog does not state.
			best, ambiguous := -1, false
			for _, v := range s.ImagePrices.Variants {
				if v.Size != "" && v.Size != req.size || v.Quality != "" && v.Quality != req.quality {
					continue
				}
				specificity := 0
				if v.Size != "" {
					specificity++
				}
				if v.Quality != "" {
					specificity++
				}
				switch {
				case specificity > best:
					best, ambiguous = specificity, false
					rate, _ = decimalRate(v.Price)
				case specificity == best:
					ambiguous = true
				}
			}
			if ambiguous || rate == nil {
				return 0, false, "unknown"
			}
		} else if s.Image != nil {
			rate, _ = decimalRate(strconv.FormatFloat(*s.Image, 'f', -1, 64))
		}
		if rate != nil {
			rate.Mul(rate, big.NewRat(int64(u.Images), 1))
			f, _ := rate.Float64()
			return f, true, "calculated"
		}
	}
	if !u.TokensKnown {
		return 0, false, "unknown"
	}
	total := new(big.Rat)
	add := func(n uint64, rate *string) bool {
		if n == 0 {
			return true
		}
		if rate == nil {
			return false
		}
		r, ok := decimalRate(*rate)
		if !ok {
			return false
		}
		r.Mul(r, new(big.Rat).SetInt(new(big.Int).SetUint64(n)))
		r.Quo(r, big.NewRat(1_000_000, 1))
		total.Add(total, r)
		return true
	}
	if operation == "images" && s.ImagePrices != nil && (s.ImagePrices.TextInput != nil || s.ImagePrices.ImageInput != nil || s.ImagePrices.TextOutput != nil || s.ImagePrices.ImageOutput != nil) {
		if !u.BreakdownKnown || u.Cache > 0 {
			return 0, false, "unknown"
		}
		p := s.ImagePrices
		if !add(u.TextInput, p.TextInput) || !add(u.ImageInput, p.ImageInput) || !add(u.TextOutput, p.TextOutput) || !add(u.ImageOutput, p.ImageOutput) {
			return 0, false, "unknown"
		}
	} else {
		if s.Input == "" || (operation == "images" && s.Output == "") {
			return 0, false, "unknown"
		}
		if !add(u.Input-u.Cache, &s.Input) || !add(u.Cache, &s.Cache) || !add(u.Output, &s.Output) {
			return 0, false, "unknown"
		}
	}
	f, _ := total.Float64()
	return f, true, "calculated"
}
