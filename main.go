package main

import (
	"bufio"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nlopes/slack"
)

const (
	limit      = 1000
	startToken = "^"
	endToken   = "$"
	chainFile  = "markov_chain"
)

var (
	userToken        = flag.String("user-token", "", "Slack user token")
	botToken         = flag.String("bot-token", "", "Slack bot token")
	email            = flag.String("email", "", "Email address of slack user to create bot for")
	optionMin        = flag.Int("option-min", 2, "Minimum number of options before downgrading to shorter prefix")
	prefixMax        = flag.Int("prefix-max", 5, "Maximum prefix length")
	prefixMin        = flag.Int("prefix-min", 2, "Minimum prefix length, even if below option-min")
	sentenceLen      = flag.Int("sentence-length", 10, "Target sentence length")
	sentenceAttempts = flag.Int("sentence-attempts", 5, "Number of times to try building a sentence longer than minimum")
	stopwordsFile    = flag.String("stopwords", "./stopwords.txt", "Stopwords file")
	cache            = flag.String("cache", "./cache", "Cache directory")
	buffer           = flag.Int("buffer", 1000, "Buffer size")
	concurrency      = flag.Int("concurrency", 3, "Concurrency")
)

var cacheDir string

func main() {
	flag.Parse()
	rand.Seed(time.Now().UnixNano())

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

	if *cache != "" {
		cacheDir = fmt.Sprintf("%s/%s", *cache, user.Profile.Email)
		if err := os.MkdirAll(*cache, 0755); err != nil {
			log.Fatal("Error creating cache directory: %s", err)
		}
	}

	stopwords := readStopwords()
	channels := fetchChannels(userClient)
	msgs := fetchChannelHistories(userClient, user, channels)
	chain := buildMarkovChain(msgs)
	startBot(botClient, chain, stopwords)

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

	filename := fmt.Sprintf("%s/%s.txt", cacheDir, channelName)
	if cacheDir != "" {
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

	if cacheDir != "" {
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

	for msg := range msgs {
		tokens := strings.FieldsFunc(msg, func(r rune) bool {
			if unicode.IsSpace(r) {
				return true
			}
			switch r {
			case ';', '[', ']', '{', '}', '(', ')', '|', ',', '"':
				return true
			default:
				return false
			}
		})

		prefixes := [][]string{}
		for i := 0; i < *prefixMax; i++ {
			prefixes = append(prefixes, []string{startToken})
		}

		for _, token := range tokens {
			if !strings.ContainsAny(strings.ToLower(token), "abcdefghijklmnopqrstuvwxyz1234567890") {
				continue
			}
			for i := 0; i < *prefixMax; i++ {
				prefix := prefixes[i]
				if len(prefix) > i {
					key := strings.ToLower(strings.Join(prefix, " "))
					chain[key] = append(chain[key], token)
				}

				prefix = append(prefix, token)
				if len(prefix) > i+1 {
					prefix = prefix[1:]
				}
				prefixes[i] = prefix
			}
		}
		for i := 0; i < *prefixMax; i++ {
			prefix := prefixes[i]
			if len(prefix) > i {
				key := strings.ToLower(strings.Join(prefix, " "))
				chain[key] = append(chain[key], endToken)
			}
		}
	}

	log.Printf("Markov chain built!")
	return chain
}

func (c MarkovChain) Generate(input string, stopwords map[string]bool) string {
	// Choose starting prefix
	var (
		words  = shuffledWords(input, stopwords)
		prefix = []string{startToken}
		out    []string
	)
	// Only use input word 50% of time if user gave few words
	if len(words) > 3 || rand.Int31n(2) > 0 {
		for _, word := range words {
			if len(c[word]) > *optionMin {
				log.Printf("Using input word as prefix: %s", word)
				prefix = []string{startToken, word}
				out = append(out, word)
				break
			}
		}
	}

	var attempts int
	for {
		key := strings.ToLower(strings.Join(prefix, " "))
		opts := c[key]

		log.Printf("Key: '%s'; Prefix: %d; Opts: %d", key, len(prefix), len(opts))
		if len(opts) == 0 || (len(opts) < *optionMin && len(prefix) > *prefixMin && len(out) < *sentenceLen) {
			log.Printf("Too few options: %d; Prefix length: %d", len(opts), len(prefix))
			prefix = prefix[1:] // Use a shorter prefix
			continue
		}

		word := opts[rand.Intn(len(opts))]
		if word == endToken {
			if len(out) < *sentenceLen && len(opts) > 1 && attempts < *sentenceAttempts {
				attempts++
				log.Printf("Below minimum sentence length - trying again")
				continue
			}
			return strings.Join(out, " ")
		} else if (strings.HasSuffix(word, ".") ||
			strings.HasSuffix(word, "!") ||
			strings.HasSuffix(word, "?")) &&
			strings.Count(word, ".") <= 1 { // Abbreviations shouldn't end sentences
			if len(out)+1 < *sentenceLen && len(opts) > 1 && attempts < *sentenceAttempts {
				attempts++
				log.Printf("Below minimum sentence length - trying again")
				continue
			}
			out = append(out, word)
			return strings.Join(out, " ")
		}
		prefix = append(prefix, word)
		if len(prefix) > *prefixMax {
			prefix = prefix[1:]
		}
		out = append(out, word)
	}
}

func readStopwords() map[string]bool {
	log.Println("Reading stopwords file: %s", *stopwordsFile)
	file, err := os.Open(*stopwordsFile)
	if err != nil {
		log.Fatalf("Error opening stopwords file: %s", err)
	}
	defer file.Close()

	stopwords := map[string]bool{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		stopwords[scanner.Text()] = true
	}
	if err := scanner.Err(); err != nil {
		log.Fatalf("Error scanning cache file: %s", err)
	}

	return stopwords
}

func shuffledWords(text string, stopwords map[string]bool) []string {
	input := strings.FieldsFunc(text, func(r rune) bool {
		if unicode.IsSpace(r) {
			return true
		}
		switch r {
		case ';', '[', ']', '{', '}', '(', ')', '|', ',', '"', '\'', '.', '?', '!':
			return true
		default:
			return false
		}
	})

	var words []string
	for _, word := range input {
		if stopwords[word] {
			continue
		}
		words = append(words, word)
	}
	rand.Shuffle(len(words), func(i, j int) {
		words[i], words[j] = words[j], words[i]
	})
	return words
}

func startBot(botClient *slack.Client, chain MarkovChain, stopwords map[string]bool) {
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

			log.Printf("Message received: %v\n", ev.Text)
			rtm.SendMessage(rtm.NewTypingMessage(ev.Channel))
			response := chain.Generate(ev.Text, stopwords)
			time.Sleep(time.Second / 3)
			log.Printf("Response: %v\n", response)
			rtm.SendMessage(rtm.NewOutgoingMessage(
				response,
				ev.Channel,
			))
			log.Printf("Message successfully sent to channel %ss", ev.Channel)
		//	channelID, timestamp, err := botClient.PostMessage(
		//		ev.Channel,
		//		slack.MsgOptionText(response, false),
		//	)
		//	if err != nil {
		//		log.Printf("Error posting message: %s\n", err)
		//	} else {
		//		log.Printf("Message successfully sent to channel %s at %s", channelID, timestamp)
		//	}
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
