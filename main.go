package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nlopes/slack"
)

const (
	limit      = 1000
	startToken = "^"
	endToken   = "$"
	chainFile  = "chain"
)

var (
	userToken   = flag.String("user-token", "", "Slack user token")
	botToken    = flag.String("bot-token", "", "Slack bot token")
	email       = flag.String("email", "", "Email address of slack user to create bot for")
	cache       = flag.String("cache", "./cache", "Cache directory")
	buffer      = flag.Int("buffer", 1000, "Buffer size")
	concurrency = flag.Int("concurrency", 3, "Concurrency")
)

func main() {
	flag.Parse()
	rand.Seed(time.Now().UnixNano())

	if *cache != "" {
		if err := os.MkdirAll(*cache, 0755); err != nil {
			log.Fatal("Error creating cache directory: %s", err)
		}
	}

	userClient := slack.New(*userToken)
	botClient := slack.New(*botToken)

	log.Println("Authenticating with user token")
	if _, err := userClient.AuthTest(); err != nil {
		log.Fatalf("Error authenticating with user token: %s", err)
	}

	log.Println("Authenticating with bot token")
	if _, err := botClient.AuthTest(); err != nil {
		log.Fatalf("Error authenticating with bot token: %s", err)
	}

	log.Printf("Fetching user info for user: %s", *email)
	user, err := userClient.GetUserByEmail(*email)
	if err != nil {
		log.Fatalf("Error fetching user by email: %s", err)
	}

	channels := fetchChannels(userClient)
	msgs := fetchChannelHistories(userClient, user, channels)
	chain := buildMarkovChain(msgs)
	startBot(botClient, chain)

	log.Printf("Goodbye!")
}

func fetchChannels(client *slack.Client) <-chan slack.Channel {
	log.Println("Fetching list of channels")
	var (
		channels = make(chan slack.Channel, *buffer)
		params   = &slack.GetConversationsParameters{
			Types: []string{"public_channel", "private_channel", "mpim", "im"},
			Limit: limit,
		}
	)
	go func() {
		defer close(channels)
		for {
			chans, cursor, err := client.GetConversations(params)
			if err != nil {
				log.Fatalf("Error getting conversations: %s", err)
			}
			for _, c := range chans {
				channels <- c
			}
			if cursor == "" {
				break
			}
			params.Cursor = cursor
		}
	}()
	return channels
}

func fetchChannelHistories(client *slack.Client, user *slack.User, channels <-chan slack.Channel) <-chan string {
	log.Println("Fetching channel histories")
	var (
		msgs = make(chan string, *buffer)
		wg   sync.WaitGroup
	)
	wg.Add(*concurrency)
	for i := 0; i < *concurrency; i++ {
		go func() {
			defer wg.Done()
			for channel := range channels {
				fetchChannelHistory(client, user, channel, msgs)
			}
		}()
	}
	go func() {
		wg.Wait()
		close(msgs)
	}()

	return msgs
}

func fetchChannelHistory(client *slack.Client, user *slack.User, channel slack.Channel, msgs chan<- string) {
	var channelName string
	if channel.Name != "" {
		channelName = channel.Name
	} else {
		channelName = channel.ID
	}

	log.Printf("Fetching channel history: %s", channelName)

	filename := fmt.Sprintf("%s/%s.txt", *cache, channelName)
	if *cache != "" {
		if file, err := os.Open(filename); err != nil {
			log.Printf("Error opening cache file: %s", err)
		} else {
			log.Printf("Using cache file: %s", filename)
			defer file.Close()

			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				msgs <- scanner.Text()
			}
			if err := scanner.Err(); err != nil {
				log.Fatalf("Error scanning cache file: %s", err)
			}
			return
		}
	}

	var (
		page     = 1
		messages []string
		params   = &slack.GetConversationHistoryParameters{
			ChannelID: channel.ID,
			Limit:     limit,
		}
	)
	for {
		log.Printf("%s - page %d", channelName, page)

		resp, err := client.GetConversationHistory(params)
		if err, ok := err.(*slack.RateLimitedError); ok {
			retryAfter := err.RetryAfter * time.Duration(*concurrency)
			log.Printf("Rate limited. Retrying after: %s", retryAfter)
			time.Sleep(retryAfter)
			continue
		} else if err != nil {
			log.Fatalf("%s - Error getting conversation history: %s", channelName, err)
		} else if resp == nil {
			log.Printf("%s - nil response when getting conversation history", channelName)
			break
		}

		// Filter messages by user
		for _, msg := range resp.Messages {
			if msg.User != user.ID {
				continue
			}
			msgs <- msg.Text
			messages = append(messages, msg.Text)
		}

		if !resp.HasMore {
			break
		}
		params.Cursor = resp.ResponseMetaData.NextCursor
		page++
	}

	if *cache != "" {
		log.Printf("Saving to cache: %s", filename)
		out := strings.Join(messages, "\n")
		if err := ioutil.WriteFile(filename, []byte(out), 0755); err != nil {
			log.Fatalf("Error writing file: %s", err)

		}
	}
}

type MarkovChain map[string][]string

func buildMarkovChain(msgs <-chan string) MarkovChain {
	chain := MarkovChain{}
	filename := fmt.Sprintf("%s/%s.json", *cache, chainFile)
	if *cache != "" {
		if body, err := ioutil.ReadFile(filename); err == nil {
			log.Printf("Using cached file: %s", filename)
			if err := json.Unmarshal(body, &chain); err != nil {
				log.Fatalf("Error unmarshaling markov chain file: %s", err)
			}
			go func() {
				for range msgs {
				}
			}()
			return chain
		} else {
			log.Printf("Error reading markov chain file: %s", err)
		}
	}

	for msg := range msgs {
		tokens := strings.Fields(msg)

		prev := startToken
		for _, token := range tokens {
			chain[strings.ToLower(prev)] = append(chain[prev], token)
			prev = token
		}
		chain[strings.ToLower(prev)] = append(chain[prev], endToken)
	}

	if *cache != "" {
		out, err := json.Marshal(chain)
		if err != nil {
			log.Fatalf("Error marshaling markov chain to JSON: %s", err)
		}
		if err := ioutil.WriteFile(filename, out, 0755); err != nil {
			log.Fatal("Error writing markov chain to cache file: %s", err)
		}
	}

	log.Printf("Markov chain built!")
	return chain
}

func (c MarkovChain) Generate() string {
	var out []string

	prev := startToken
	for {
		opts := c[prev]
		choice := opts[rand.Intn(len(opts))]
		if choice == endToken {
			if len(out) == 0 {
				continue
			}
			return strings.Join(out, " ")
		}
		prev = strings.ToLower(choice)
		out = append(out, choice)
	}
}

func startBot(botClient *slack.Client, chain MarkovChain) {
	log.Printf("Starting bot")
	rtm := botClient.NewRTM()
	go rtm.ManageConnection()

	for msg := range rtm.IncomingEvents {
		switch ev := msg.Data.(type) {
		case *slack.ConnectedEvent:
			log.Printf("Connected")
		case *slack.MessageEvent:
			// Don't respond to bot messages (including our own)
			if ev.BotID != "" {
				continue
			}

			response := chain.Generate()
			log.Printf("Message received: %v\n", ev.Text)
			log.Printf("Response: %v\n", response)
			channelID, timestamp, err := botClient.PostMessage(
				ev.Channel,
				slack.MsgOptionText(response, false),
			)
			if err != nil {
				log.Printf("Error posting message: %s\n", err)
			} else {
				log.Printf("Message successfully sent to channel %s at %s", channelID, timestamp)
			}
		case *slack.LatencyReport:
			log.Printf("Current latency: %v\n", ev.Value)
		case *slack.RTMError:
			log.Printf("RTM Error: %s\n", ev.Error())
		case *slack.InvalidAuthEvent:
			log.Printf("Invalid credentials")
			return
		default:
			continue
		}
	}
}
