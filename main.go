package main

import (
	"bufio"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"path"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nlopes/slack"
	"github.com/peterbourgon/ff"
)

const (
	limit             = 1000
	startToken        = "^"
	endToken          = "$"
	alphanumericChars = "abcdefghijklmnopqrstuvwxyz1234567890"
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
	minInputWords    = flag.Int("input-words", 3, "Number of input words required to definitely choose one as the starting prefix")
	stopwordsFile    = flag.String("stopwords", "./stopwords.txt", "Stopwords file")
	cache            = flag.String("cache", "./cache", "Cache directory")
	buffer           = flag.Int("buffer", 1000, "Buffer size")
	concurrency      = flag.Int("concurrency", 3, "Concurrency")
)

var cacheDir string

func main() {
	if err := ff.Parse(flag.CommandLine, os.Args[1:], ff.WithEnvVarNoPrefix()); err != nil {
		log.Fatalf("Error parsing flags: %s", err)
	}
	rand.Seed(time.Now().UnixNano())

	userClient := slack.New(*userToken)
	botClient := slack.New(*botToken)

	log.Println("Authenticating with user token")
	if _, err := userClient.AuthTest(); err != nil {
		log.Fatalf("Error authenticating with user token: %s", err)
	}

	log.Println("Authenticating with bot token")
	botInfo, err := botClient.AuthTest()
	if err != nil {
		log.Fatalf("Error authenticating with bot token: %s", err)
	}

	log.Printf("Fetching user info for user: %s", *email)
	user, err := userClient.GetUserByEmail(*email)
	if err != nil {
		log.Fatalf("Error fetching user by email: %s", err)
	}

	if *cache != "" {
		cacheDir = path.Join(*cache, user.Profile.Email)
		if err := os.MkdirAll(*cache, 0755); err != nil {
			log.Fatal("Error creating cache directory: %s", err)
		}
	}

	stopwords := readStopwords()
	channels := fetchChannels(userClient)
	msgs := fetchChannelHistories(userClient, user, channels)
	chain := buildMarkovChain(msgs)
	startBot(botClient, botInfo.UserID, chain, stopwords)

	log.Printf("Goodbye!")
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
				log.Fatalf("Error getting channels: %s", err)
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

	filename := path.Join(cacheDir, fmt.Sprintf("%s.txt", channelName))
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
			log.Fatalf("%s - Error getting channel history: %s", channelName, err)
		} else if resp == nil {
			log.Printf("%s - nil response when getting channel history", channelName)
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

type Prefix []string

func (p Prefix) String() string {
	return strings.ToLower(strings.Join(p, " "))
}

func buildMarkovChain(msgs <-chan string) MarkovChain {
	chain := MarkovChain{}

	for msg := range msgs {
		// All prefixes start with the start token
		prefixes := []Prefix{}
		for i := 0; i < *prefixMax; i++ {
			prefixes = append(prefixes, Prefix{startToken})
		}

		// Split the message into tokens, index each token by its prefix
		tokens := splitMessage(msg)
		for _, token := range tokens {
			// Skip tokens with no alphanumeric characters (probably code fragments)
			if !strings.ContainsAny(strings.ToLower(token), alphanumericChars) {
				continue
			}

			// Index token by all prefixes, up to prefixMax
			for i := 0; i < *prefixMax; i++ {
				prefix := prefixes[i]

				// Index the token if the prefix has reached its predefined
				// length (otherwise, we'd be duplicating shorter prefixes)
				if len(prefix) > i {
					key := prefix.String()
					chain[key] = append(chain[key], token)
				}

				// Update the prefix to include the new token. If the prefix
				// has surpassed its predefined length, remove the first token
				// (shift the sliding window)
				prefix = append(prefix, token)
				if len(prefix) > i+1 {
					prefix = prefix[1:]
				}
				prefixes[i] = prefix
			}
		}

		// Index the endToken by all prefixes
		for i := 0; i < *prefixMax; i++ {
			prefix := prefixes[i]
			if len(prefix) > i {
				key := prefix.String()
				chain[key] = append(chain[key], endToken)
			}
		}
	}

	log.Printf("Markov chain built!")
	return chain
}

// Splits a message into tokens, for sake of building markov chain
func splitMessage(msg string) []string {
	return strings.FieldsFunc(msg, func(r rune) bool {
		// Split on whitespace
		if unicode.IsSpace(r) {
			return true
		}

		// Split on characters that don't tend to work well in markov chains,
		// but leave end-punctuation ('.', '!', '?'), which is used to
		// terminate generated sentences along with the endToken
		switch r {
		case ';', '[', ']', '{', '}', '(', ')', '|', ',', '"':
			return true
		default:
			return false
		}
	})
}

func (c MarkovChain) Generate(input string, stopwords map[string]bool) string {
	var (
		// Choose starting prefix from input, start generating sentence
		prefix, out = c.startingPrefix(input, stopwords)
		attempts    int
	)
	for {
		// Get all of the word options for this prefix
		key := prefix.String()
		opts := c[key]

		// Check if we should use a shorter prefix (because this prefix isn't
		// generating enough options in the markov chain, and therefore
		// wouldn't generate a very novel sentence)
		log.Printf("Prefix: '%s'; Prefix Length: %d; Opts: %d", key, len(prefix), len(opts))
		if useShorterPrefix(prefix, opts, out) {
			log.Printf("Too few options: %d; Prefix length: %d", len(opts), len(prefix))
			prefix = prefix[1:] // Use a shorter prefix
			continue
		}

		// Randomly choose a word from the available options. If it's the
		// endToken, or a word that ends with punctuation, end the sentence
		// (unless we haven't reached the target sentence length yet, and there
		// are other potential options that don't end the sentence, in which
		// case, we try again)
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

		// Append chosen word to sentence
		out = append(out, word)

		// Add the chosen word to the prefix, and shift the
		// sliding window if we've passed max prefix length
		prefix = append(prefix, word)
		if len(prefix) > *prefixMax {
			prefix = prefix[1:]
		}
	}
}

func (c MarkovChain) startingPrefix(input string, stopwords map[string]bool) (Prefix, []string) {
	// Choose a starting prefix from the chat message used to trigger the bot
	var (
		words  = startWords(input, stopwords)
		prefix = Prefix{startToken}
		out    []string
	)
	// Only use input word 50% of time if the user didn't give many words
	// (otherwise, it would be too predictable)
	if len(words) > *minInputWords || rand.Int31n(2) > 0 {
		// For each valid input word, check if it's a known prefix
		for _, word := range words {
			// If the word is a known prefix with at least the minimum number
			// of options in the markov chain, use it
			if len(c[word]) > *optionMin {
				log.Printf("Using input word as prefix: %s", word)
				prefix = Prefix{startToken, word}
				out = append(out, word)
				break
			}
		}
	}
	return prefix, out
}

func startWords(text string, stopwords map[string]bool) []string {
	// Split input message (sent from user to trigger bot response) into tokens
	// for sake of finding work to start markov chain response
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

	// Filter out stopwords
	var words []string
	for _, word := range input {
		if word == "" {
			continue
		}
		if stopwords[word] {
			continue
		}
		words = append(words, word)
	}

	// Randomize order of words
	rand.Shuffle(len(words), func(i, j int) {
		words[i], words[j] = words[j], words[i]
	})
	return words
}

func useShorterPrefix(prefix Prefix, opts []string, out []string) bool {
	// If we have no options, use a shorter prefix (this can happen if we
	// choose a starting prefix with an input word that never actually starts a
	// sentence)
	if len(opts) == 0 {
		return true
	}

	// If we have less than optionMin options, and our prefix is greater than
	// prefixMin, and our sentence hasn't reached the target length yet, use a
	// shorter prefix (if the sentence has reached the target length, we just
	// let it finish in a coherent way, rather than increasing randomness by
	// shortening the prefix, which leads to more nonsensical sentences)
	if len(opts) < *optionMin &&
		len(prefix) > *prefixMin &&
		len(out) < *sentenceLen {
		return true
	}
	return false
}

func startBot(botClient *slack.Client, botID string, chain MarkovChain, stopwords map[string]bool) {
	log.Printf("Starting bot")
	rtm := botClient.NewRTM()
	go rtm.ManageConnection()

	channels := map[string]slack.Channel{}
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

			// Fetch channel the message was from (cache for future reference)
			channel, exists := channels[ev.Channel]
			if !exists {
				c, err := botClient.GetConversationInfo(ev.Channel, false)
				if err != nil {
					log.Printf("Error getting channel info: %s", err)
					continue
				}
				channels[c.ID] = *c
				channel = *c
			}

			// Only respond to DMs, or if the bot was mentioned
			if !(channel.IsIM || strings.Contains(ev.Text, botID)) {
				log.Printf("Skipping - not a DM, and doesn't mention bot")
				continue
			}

			rtm.SendMessage(rtm.NewTypingMessage(ev.Channel))
			response := chain.Generate(ev.Text, stopwords)
			time.Sleep(time.Second / 3) // Mimicks typing
			rtm.SendMessage(rtm.NewOutgoingMessage(
				response,
				ev.Channel,
			))
			log.Printf("Message sent: %v\n", response)
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
