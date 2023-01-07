package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/datastore"
	"github.com/dghubble/go-twitter/twitter"
	"github.com/dghubble/oauth1"
	runtimeconfig "google.golang.org/api/runtimeconfig/v1beta1"
)

const stateEntity = "State"
const stateID = stateEntity

type State struct {
	LastProcessedID string
}

func LoadState(ctx context.Context, ds *datastore.Client) (*State, error) {
	r := &State{}
	if err := ds.Get(ctx, datastore.NameKey(stateEntity, stateID, nil), r); err != nil {
		if err == datastore.ErrNoSuchEntity {
			return r, nil
		}
		return nil, fmt.Errorf("failed to get state: %w", err)
	}
	return r, nil
}

func (s *State) Save(ctx context.Context, ds *datastore.Client) error {
	if _, err := ds.Put(ctx, datastore.NameKey(stateEntity, stateID, nil), s); err != nil {
		return fmt.Errorf("failed to store state: %w", err)
	}
	return nil
}

func stringify(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return string(b)
}

func PollDMs(ctx context.Context, ds *datastore.Client) error {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	if err := pollDMsOnce(ctx, ds); err != nil {
		log.Printf("Failed to poll DMs: %s", err)
	}
	for {
		select {
		case <-t.C:
			if err := pollDMsOnce(ctx, ds); err != nil {
				log.Printf("Failed to poll DMs: %s", err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func cmpID(a string, b string) bool {
	if len(a) == len(b) {
		return a < b
	}
	return len(a) < len(b)
}

func twitterClient(appCreds *TwitterCredentials, userCreds *TwitterUserCredentials) *twitter.Client {
	config := oauth1.NewConfig(appCreds.APIKey, appCreds.APIKeySecret)
	token := oauth1.NewToken(userCreds.Token, userCreds.TokenSecret)
	httpClient := config.Client(oauth1.NoContext, token)

	return twitter.NewClient(httpClient)
}

func pollDMsOnce(ctx context.Context, ds *datastore.Client) error {
	log.Printf("Polling DMs")

	rcService, err := runtimeconfig.NewService(ctx)
	if err != nil {
		return err
	}
	vars := rcService.Projects.Configs.Variables
	senderWhitelist := map[string]string{}
	err = vars.List(fmt.Sprintf("projects/%s/configs/prod", os.Getenv("GOOGLE_CLOUD_PROJECT"))).
		Filter(fmt.Sprintf("projects/%s/configs/prod/variables/whitelist/", os.Getenv("GOOGLE_CLOUD_PROJECT"))).
		PageSize(1000).
		ReturnValues(true).
		Pages(ctx, func(resp *runtimeconfig.ListVariablesResponse) error {
			prefix := fmt.Sprintf("projects/%s/configs/prod/variables/whitelist/", os.Getenv("GOOGLE_CLOUD_PROJECT"))
			for _, v := range resp.Variables {
				if !strings.HasPrefix(v.Name, prefix) {
					continue
				}
				senderWhitelist[v.Text] = strings.TrimPrefix(v.Name, prefix)
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("fetching whitelist: %w", err)
	}
	log.Printf("Whitelisted users (%d total): %#v", len(senderWhitelist), senderWhitelist)

	userCreds := &TwitterUserCredentials{}
	if err := ds.Get(ctx, datastore.NameKey(credentialsEntity, credentialsID, nil), userCreds); err != nil {
		return fmt.Errorf("failed to get user token: %w", err)
	}

	appCreds, err := creds(ctx)
	if err != nil {
		return fmt.Errorf("failed to get app credentials: %w", err)
	}

	state, err := LoadState(ctx, ds)
	if err != nil {
		return fmt.Errorf("reading stored state: %w", err)
	}
	log.Printf("Last processed ID: %q", state.LastProcessedID)

	twClient := twitterClient(&appCreds, userCreds)

	events := []twitter.DirectMessageEvent{}
	cursor := ""
loop:
	for {
		resp, _, err := twClient.DirectMessages.EventsList(&twitter.DirectMessageEventsListParams{Cursor: cursor, Count: 50})
		if err != nil {
			if apiError, ok := err.(twitter.APIError); ok {
				if len(apiError.Errors) > 0 && apiError.Errors[0].Code == 88 {
					// Throttled
					log.Printf("Throttled, sleeping for a bit")
					time.Sleep(15 * time.Minute)
					continue
				}
			}
			return fmt.Errorf("failed to fetch DMs: %w", err)
		}
		cursor = resp.NextCursor

		log.Printf("Got %d events", len(resp.Events))

		for _, e := range resp.Events {
			if !cmpID(state.LastProcessedID, e.ID) {
				break loop
			}
			log.Printf("%s from %s", e.ID, e.Message.SenderID)
			if e.Type != "message_create" {
				continue
			}
			_, ok := senderWhitelist[e.Message.SenderID]
			if !ok {
				continue
			}
			events = append(events, e)
		}

		if cursor == "" {
			break
		}
	}
	log.Printf("DMs fetched")

	sort.Slice(events, func(i, j int) bool { return cmpID(events[i].ID, events[j].ID) })

	for _, e := range events {
		log.Printf("Message %s from %s: %q", e.ID, e.Message.SenderID, e.Message.Data.Text)
		tid := tweetIDFromDM(e.Message)
		if tid != "" {
			log.Printf("Tweet ID detected: %s", tid)
			id, err := strconv.ParseInt(tid, 10, 64)
			if err == nil {
				log.Printf("Retweeting %q...", tid)
				if _, _, err := twClient.Statuses.Retweet(id, nil); err != nil {
					log.Printf("Failed to retweet %q: %s", tid, err)
				}
			}
		}
		state.LastProcessedID = e.ID
		if err := state.Save(ctx, ds); err != nil {
			log.Printf("Failed to save state: %s", err)
		}
	}
	return nil
}

var tweetIDRe = regexp.MustCompile("^https://twitter.com/[^/]+/status/([0-9]+)([^0-9].*)?$")

func tweetIDFromDM(msg *twitter.DirectMessageEventMessage) string {
	for _, u := range msg.Data.Entities.Urls {
		m := tweetIDRe.FindStringSubmatch(u.ExpandedURL)
		if m == nil {
			continue
		}
		return m[1]
	}
	return ""
}
