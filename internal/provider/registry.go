package provider

import (
	"context"
	"errors"

	"iptv-manager/internal/core"
)

// Register a new broadcaster here; web/store/media consume the same registry.
var definitions = []definition{tdmDefinition(), tvbDefinition(), hkstvDefinition()}

func Catalogs() []Catalog {
	result := make([]Catalog, 0, len(definitions))
	for _, d := range definitions {
		c := d.catalog
		c.Channels = append([]Channel(nil), c.Channels...)
		result = append(result, c)
	}
	return result
}

func Lookup(id string) (Catalog, bool) {
	for _, d := range definitions {
		if d.catalog.ID == id {
			c := d.catalog
			c.Channels = append([]Channel(nil), c.Channels...)
			return c, true
		}
	}
	return Catalog{}, false
}

func ChannelInfo(id, key string) (Channel, bool) {
	c, ok := Lookup(id)
	if !ok {
		return Channel{}, false
	}
	for _, ch := range c.Channels {
		if ch.ID == key {
			return ch, true
		}
	}
	return Channel{}, false
}

// Normalize accepts registered identities only, never arbitrary provider URLs.
func Normalize(ch core.Channel) (core.Channel, error) {
	item, ok := ChannelInfo(ch.ProviderID, ch.ProviderChannelID)
	if !ok || ch.SubscriptionID != "" {
		return ch, errors.New("内置来源或频道标识无效")
	}
	ch.SourceType = "builtin"
	ch.URL = item.URL
	ch.Quality = 0
	return ch, nil
}

// UpgradeLegacy identifies only known official URLs. Subscription ownership and
// unrelated/custom streams are preserved, regardless of their display names.
func UpgradeLegacy(ch core.Channel) core.Channel {
	if ch.SubscriptionID != "" || ch.ProviderID != "" || ch.ProviderChannelID != "" {
		return ch
	}
	for _, d := range definitions {
		key := ""
		if d.matchLegacy != nil {
			key = d.matchLegacy(ch.SourceType, ch.URL)
		} else if ch.SourceType == "stream" {
			for _, item := range d.catalog.Channels {
				if item.URL == ch.URL {
					key = item.ID
					break
				}
			}
		}
		if key != "" {
			ch.SourceType = "builtin"
			ch.ProviderID = d.catalog.ID
			ch.ProviderChannelID = key
			return ch
		}
	}
	return ch
}

type Registry struct{ resolvers map[string]resolver }

func New(opts Options) *Registry {
	r := &Registry{resolvers: make(map[string]resolver)}
	for _, d := range definitions {
		r.resolvers[d.catalog.ID] = d.create(opts)
	}
	return r
}
func (r *Registry) Resolve(ctx context.Context, ch core.Channel, settings core.Settings, config string) (Playback, error) {
	normalized, err := Normalize(ch)
	if err != nil {
		return Playback{}, err
	}
	p := r.resolvers[ch.ProviderID]
	if p == nil {
		return Playback{}, errors.New("内置来源不可用")
	}
	return p.Resolve(ctx, normalized, settings, config)
}
func (r *Registry) Invalidate(id string) {
	for _, p := range r.resolvers {
		p.Invalidate(id)
	}
}
