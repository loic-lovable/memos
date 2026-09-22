package shrimp

import (
	_ "embed"
	"encoding/json"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/humanattributes"
	"github.com/pkg/errors"
)

//go:embed tzdb2025b.json
var timezoneNames []byte

func experimentalHumanProfile(enabled bool) (*humanattributes.Profile, error) {
	if !enabled {
		return nil, nil
	}
	if !experimentalHumanAttributesAvailable {
		return nil, errors.New("human attributes require the disposable shrimptest build")
	}
	var names []string
	if err := json.Unmarshal(timezoneNames, &names); err != nil {
		return nil, err
	}
	return humanattributes.New(humanattributes.Config{MaxEmails: 16, TZDBVersion: "2025b", Timezones: names})
}
