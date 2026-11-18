package controlapp

import (
	"errors"
	"path/filepath"
	"strconv"

	"github.com/zyno-io/thinpost/internal/tracking"
)

type TrackingConfig struct {
	Origin     string
	Links      *tracking.LinkKeys
	EventLimit int
}

func loadTracking(getenv func(string) string) (*TrackingConfig, error) {
	keys := []string{"THINPOST_TRACKING_ORIGIN", "THINPOST_TRACKING_LINK_KEYS_FILE", "THINPOST_TRACKING_EVENT_LIMIT"}
	if getenv("THINPOST_TRACKING_QUEUE_WINDOW") != "" || getenv("THINPOST_TRACKING_POST_SEND_VALIDITY") != "" {
		return nil, errors.New("tracking links have no configured expiration")
	}
	configured := false
	for _, key := range keys {
		configured = configured || getenv(key) != ""
	}
	if !configured {
		return nil, nil
	}
	if getenv("THINPOST_CONTROL_FINGERPRINT_KEY_FILE") == "" && getenv("THINPOST_CONTROL_FINGERPRINT_KEYS_FILE") == "" {
		return nil, errors.New("tracking requires the submission fingerprint key configuration")
	}
	c := &TrackingConfig{}
	var err error
	c.Origin, err = tracking.NormalizeOrigin(getenv(keys[0]))
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(getenv(keys[1])) {
		return nil, errors.New("tracking link key file must be absolute")
	}
	c.Links, err = tracking.LoadLinkKeys(getenv(keys[1]))
	if err != nil {
		return nil, err
	}
	c.EventLimit, err = strconv.Atoi(getenv(keys[2]))
	if err != nil || c.EventLimit < 1 || c.EventLimit > 1000 {
		return nil, errors.New("invalid tracking event limit")
	}
	return c, nil
}
