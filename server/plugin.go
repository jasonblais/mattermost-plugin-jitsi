package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cristalhq/jwt/v2"
	"github.com/mattermost/mattermost-server/v5/model"
	"github.com/mattermost/mattermost-server/v5/plugin"
	"github.com/pkg/errors"
)

const (
	POST_MEETING_KEY = "post_meeting_"
)

type Plugin struct {
	plugin.MattermostPlugin

	// configurationLock synchronizes access to the configuration.
	configurationLock sync.RWMutex

	// configuration is the active plugin configuration. Consult getConfiguration and
	// setConfiguration for usage.
	configuration *configuration
}

func (p *Plugin) OnActivate() error {
	config := p.getConfiguration()
	if err := config.IsValid(); err != nil {
		return err
	}

	if err := p.API.RegisterCommand(&model.Command{
		Trigger:          jitsiCommand,
		AutoComplete:     true,
		AutoCompleteDesc: "Start a Jitsi meeting for the current channel. Optionally, append desired meeting topic after the command",
	}); err != nil {
		return err
	}

	return nil
}

// Claims extents cristalhq/jwt standard claims to add jitsi-web-token specific fields
type Claims struct {
	jwt.StandardClaims
	Room string `json:"room,omitempty"`
}

func (p *Plugin) startMeeting(user *model.User, channel *model.Channel, meetingTopic string, personal bool) (string, error) {
	var meetingID string
	meetingID = encodeJitsiMeetingID(meetingTopic)

	if len(meetingTopic) < 1 {
		namingScheme := p.getConfiguration().JitsiNamingScheme

		var channelName string
		var teamName string

		if namingScheme == "teamchannel" || namingScheme == "teamchannel-salt" {
			meetingTopic = channel.DisplayName + " Meeting"
			channelName = channel.Name
			teamName = ""
			if channel.Type == model.CHANNEL_DIRECT {
				channelName = channel.Name
				meetingTopic = user.GetDisplayName(model.SHOW_NICKNAME_FULLNAME) + "'s Meeting"
			} else if channel.Type == model.CHANNEL_GROUP {
				channelName = channel.Name
				meetingTopic = channel.DisplayName + " Meeting"
			} else {
				team, teamErr := p.API.GetTeam(channel.TeamId)
				if teamErr != nil {
					teamName = "unknown-team"
				} else {
					teamName = team.Name
				}
			}
		}

		meetingID = generateNameFromSelectedScheme(namingScheme, teamName, channelName)
	}
	jitsiURL := strings.TrimSpace(p.getConfiguration().JitsiURL)
	jitsiURL = strings.TrimRight(jitsiURL, "/")
	meetingURL := jitsiURL + "/" + meetingID

	var meetingLinkValidUntil = time.Time{}
	JWTMeeting := p.getConfiguration().JitsiJWT

	if JWTMeeting {
		signer, err2 := jwt.NewSignerHS(jwt.HS256, []byte(p.getConfiguration().JitsiAppSecret))
		if err2 != nil {
			log.Printf("Error generating new HS256 signer: %v", err2)
			return "", errors.New("Internal error")
		}
		builder := jwt.NewBuilder(signer)

		// Error check is done in configuration.IsValid()
		jURL, _ := url.Parse(p.getConfiguration().JitsiURL)

		meetingLinkValidUntil = time.Now().Add(time.Duration(p.getConfiguration().JitsiLinkValidTime) * time.Minute)

		claims := Claims{}
		claims.Issuer = p.getConfiguration().JitsiAppID
		claims.Audience = []string{p.getConfiguration().JitsiAppID}
		claims.ExpiresAt = jwt.NewNumericDate(meetingLinkValidUntil)
		claims.Subject = jURL.Hostname()
		claims.Room = meetingID

		token, err2 := builder.Build(claims)
		if err2 != nil {
			log.Printf("Error building JWT: %v", err2)
			return "", err2
		}

		meetingURL = meetingURL + "?jwt=" + string(token.Raw())
	}

	post := &model.Post{
		UserId:    user.Id,
		ChannelId: channel.Id,
		Message:   fmt.Sprintf("Meeting started at %s.", meetingURL),
		Type:      "custom_jitsi",
		Props: map[string]interface{}{
			"meeting_id":              meetingID,
			"meeting_link":            meetingURL,
			"jwt_meeting":             JWTMeeting,
			"jwt_meeting_valid_until": meetingLinkValidUntil.Format("2006-01-02 15:04:05 Z07:00"),
			"meeting_personal":        false,
			"meeting_topic":           meetingTopic,
			"from_webhook":            "true",
			"override_username":       "Jitsi",
			"override_icon_url":       "https://s3.amazonaws.com/mattermost-plugin-media/Zoom+App.png",
		},
	}

	if _, err := p.API.CreatePost(post); err != nil {
		return "", err
	}

	err := p.API.KVSet(fmt.Sprintf("%v%v", POST_MEETING_KEY, meetingID), []byte(post.Id))
	if err != nil {
		return "", err
	}

	return meetingID, nil
}

// MarshalBinary default marshaling to JSON.
func (c Claims) MarshalBinary() (data []byte, err error) {
	return json.Marshal(c)
}

func encodeJitsiMeetingID(meeting string) string {
	reg, err := regexp.Compile("[^a-zA-Z0-9]+")
	if err != nil {
		log.Fatal(err)
	}
	return reg.ReplaceAllString(meeting, "")
}
