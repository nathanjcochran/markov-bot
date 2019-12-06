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
	channelTypesStr  = flag.String("channel-types", "public_channel,private_channel,mpim,im", "Types of channels to pull messages from")
	optionMin        = flag.Int("option-min", 2, "Minimum number of options before downgrading to shorter prefix")
	prefixMax        = flag.Int("prefix-max", 5, "Maximum prefix length")
	prefixMin        = flag.Int("prefix-min", 2, "Minimum prefix length, even if below option-min")
	sentenceMin      = flag.Int("sentence-min", 4, "Target minimum sentence length")
	sentenceMax      = flag.Int("sentence-max", 12, "Target maximum sentence length")
	sentenceAttempts = flag.Int("sentence-attempts", 10, "Number of times to try building a sentence longer than minimum")
	stopwordsFile    = flag.String("stopwords", "./stopwords.txt", "Stopwords file")
	cache            = flag.String("cache", "./cache", "Cache directory")
	logDir           = flag.String("logs", "./logs", "Log directory")
	buffer           = flag.Int("buffer", 1000, "Buffer size")
	concurrency      = flag.Int("concurrency", 3, "Concurrency")
	channelTypes     []string
	cacheDir         string
)

func main() {
	if err := ff.Parse(flag.CommandLine, os.Args[1:], ff.WithEnvVarNoPrefix()); err != nil {
		log.Fatalf("Error parsing flags: %s", err)
	}
	rand.Seed(time.Now().UnixNano())
	channelTypes = strings.Split(*channelTypesStr, ",")

	userClient := slack.New(*userToken)
	botClient := slack.New(*botToken)

	log.Printf("Authenticating with user token")
	if _, err := userClient.AuthTest(); err != nil {
		log.Fatalf("Error authenticating with user token: %s", err)
	}

	log.Printf("Authenticating with bot token")
	botInfo, err := botClient.AuthTest()
	if err != nil {
		log.Fatalf("Error authenticating with bot token: %s", err)
	}

	log.Printf("Fetching user info for user: %s", *email)
	var user *slack.User
	if *email != "" {
		user, err = userClient.GetUserByEmail(*email)
		if err != nil {
			log.Fatalf("Error fetching user by email: %s", err)
		}
	}

	if *cache != "" {
		cacheDir = path.Join(*cache, botInfo.Team)
		if *email != "" {
			cacheDir = path.Join(*cache, user.Profile.Email)
		}
		if err := os.MkdirAll(cacheDir, 0755); err != nil {
			log.Fatal("Error creating cache directory: %s", err)
		}
	}

	if *logDir != "" {
		if err := os.MkdirAll(*logDir, 0755); err != nil {
			log.Fatal("Error creating logs directory: %s", err)
		}
	}

	stopwords := readStopwords()
	channels := fetchChannels(userClient, *botInfo)
	msgs := fetchChannelHistories(userClient, user, channels)
	chain := buildMarkovChain(*botInfo, msgs)
	startBot(botClient, *botInfo, chain, stopwords)

	log.Printf("Goodbye!")
}

func readStopwords() map[string]bool {
	log.Printf("Reading stopwords file: %s", *stopwordsFile)
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

func fetchChannels(client *slack.Client, botInfo slack.AuthTestResponse) <-chan slack.Channel {
	log.Printf("Fetching list of channels")
	var (
		channels = make(chan slack.Channel, *buffer)
		params   = &slack.GetConversationsParameters{
			Types: channelTypes,
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
				if c.IsIM && c.User == botInfo.UserID {
					log.Printf("Skipping DM with %s: %s", botInfo.User, c.ID)
					continue
				}
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
	log.Printf("Fetching channel histories")
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
	channelName := getChannelName(channel)
	log.Printf("Fetching channel history: %s", channelName)

	filename := path.Join(cacheDir, fmt.Sprintf("%s.txt", channelName))
	if cacheDir != "" {
		if file, err := os.Open(filename); err != nil {
			log.Printf("Error opening cache file: %s", err)
		} else {
			log.Printf("Using cache: %s", filename)
			defer file.Close()

			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				msgs <- scanner.Text()
			}
			if err := scanner.Err(); err != nil {
				log.Fatalf("Error scanning from cache file: %s", err)
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
			if user != nil && msg.User != user.ID {
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

// TODO: Name DMs after user, like log files
func getChannelName(channel slack.Channel) string {
	if channel.Name != "" {
		return channel.Name
	} else {
		return channel.ID
	}
}

type MarkovChain map[string][]string

type Prefix []string

func (p Prefix) String() string {
	return strings.ToLower(strings.Join(p, " "))
}

func buildMarkovChain(botInfo slack.AuthTestResponse, msgs <-chan string) MarkovChain {
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
			token = strings.TrimRight(token, ",")

			// Skip tokens with no alphanumeric characters (probably code fragments)
			lower := strings.ToLower(token)
			if !strings.ContainsAny(lower, alphanumericChars) {
				continue
			}
			// Skip tokens that represent a bot mention
			if strings.Contains(lower, strings.ToLower(botInfo.UserID)) {
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
		case ';', '[', ']', '{', '}', '(', ')', '|', '"':
			return true
		default:
			return false
		}
	})
}

func (c MarkovChain) Generate(input string, stopwords map[string]bool) string {
	// Choose starting prefix from input
	prefix, out := c.startingPrefix(input, stopwords)
	for {
		// Get all of the word options for the current prefix
		key := prefix.String()
		opts := c[key]

		// Check if we should use a shorter prefix (because this prefix isn't
		// generating enough options in the markov chain, and therefore
		// wouldn't generate a very novel sentence)
		log.Printf("Len: %-3d Prefix: %-3d Opts: %-5d '%s'",
			len(out), len(prefix), len(opts), key,
		)
		if useShorterPrefix(prefix, opts, out) {
			prefix = prefix[1:] // Use a shorter prefix
			continue
		}

		// Randomly choose a word from the available options. If it's the
		// endToken, or a word that ends with punctuation, end the sentence
		// (unless we haven't reached the target sentence length yet, and there
		// are other potential options that don't end the sentence, in which
		// case, try again)
		var attempts int
		for {
			word := opts[rand.Intn(len(opts))]
			if word == endToken {
				if len(out) < *sentenceMin && len(opts) > 1 && attempts < *sentenceAttempts {
					attempts++
					log.Printf("Len: %-3d Attempt: %-3d '%s'",
						len(out), attempts, strings.Join(out, " "),
					)
					continue
				}
				return strings.Join(out, " ")
			} else if (strings.HasSuffix(word, ".") ||
				strings.HasSuffix(word, "!") ||
				strings.HasSuffix(word, "?")) &&
				strings.Count(word, ".") <= 1 { // Abbreviations shouldn't end sentences
				if len(out)+1 < *sentenceMin && len(opts) > 1 && attempts < *sentenceAttempts {
					attempts++
					log.Printf("Len: %-3d Attempt: %-3d '%s'",
						len(out)+1, attempts, strings.Join(append(out, word), " "),
					)
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
			break
		}
	}
}

func (c MarkovChain) startingPrefix(input string, stopwords map[string]bool) (Prefix, []string) {
	// Jeopardy hack
	if strings.Contains(strings.ToLower(input), "play jeopardy") {
		return Prefix{startToken, "What", "is"}, []string{"What", "is"}
	}

	// Get a list of valid words from the input message
	words := startWords(input, stopwords)
	log.Printf("Potential starting words: %v", words)

	// For each valid input word, check if it's a known prefix
	for _, word := range words {
		// Starting token is always valid
		if word == startToken {
			break
		}

		// If the word is a known prefix with at least the minimum number
		// of options in the markov chain, use it
		opts := c[strings.ToLower(word)]
		if len(opts) > *optionMin {
			// Mentions look like <@USER_ID>, and have to be capitalized
			if !(strings.HasPrefix(word, "<@") && strings.HasSuffix(word, ">")) {
				word = strings.Title(word)
			}
			log.Printf("Using prefix: '%s'", word)
			return Prefix{startToken, word}, []string{word}
		}
		log.Printf("'%s' has too few options: %d",
			word, len(opts),
		)
	}
	log.Printf("Using prefix: '%s'", startToken)
	return Prefix{startToken}, []string{}
}

func startWords(input string, stopwords map[string]bool) []string {
	// Split input message (sent from user to trigger bot response) into tokens
	// for sake of finding work to start markov chain response
	var words []string
	for _, word := range splitMessage(input) {
		// Trim any punctuation on the right (we don't want to start with an
		// ending-word), except for periods if the word is an abbreviation
		word = strings.TrimRight(word, ",!?")
		if strings.Count(word, ".") < 2 {
			word = strings.TrimRight(word, ".")
		}

		// Filter out empty strings and stopwords
		if word == "" || stopwords[strings.ToLower(word)] {
			continue
		}
		words = append(words, word)
	}

	rand.Shuffle(len(words), func(i, j int) {
		words[i], words[j] = words[j], words[i]
	})
	words = append(words, startToken)
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
		len(out) < *sentenceMax {
		return true
	}
	return false
}

func startBot(botClient *slack.Client, botInfo slack.AuthTestResponse, chain MarkovChain, stopwords map[string]bool) {
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
			// TODO: Skip automated messages - e.g. "That looks like a Google Drive link"

			log.Printf("------------------------------")
			log.Printf("Message received: '%s'\n", ev.Text)
			log.Printf("Type: %s %s\n", ev.Type, ev.SubType)

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

			log.Printf("Channel: %s\n", getChannelName(channel))

			// Fetch user the message was from (cache for future reference)
			user, err := botClient.GetUserInfo(ev.User)
			if err != nil {
				log.Printf("Error getting user info for user: %s: %s", ev.User, err)
				continue
			}

			log.Printf("User: %s\n", user.Name)

			if ev.Type == "message" && ev.SubType == "channel_join" && ev.User == botInfo.UserID {
				rtm.SendMessage(rtm.NewTypingMessage(ev.Channel))
				response := "Hello!"
				time.Sleep(time.Second / 3) // Mimicks typing
				rtm.SendMessage(rtm.NewOutgoingMessage(
					response,
					ev.Channel,
				))
				log.Printf("Message sent: '%s'\n", response)

				// Log message
				if *logDir != "" {
					logMessages(channel, user.Name, botInfo.User, ev.Text, response)
				}
				continue
			}

			// Only respond to DMs, or if the bot was mentioned
			if !(channel.IsIM || strings.Contains(ev.Text, botInfo.UserID)) {
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
			log.Printf("Message sent: '%s'\n", response)

			// Log message
			if *logDir != "" {
				logMessages(channel, user.Name, botInfo.User, ev.Text, response)
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

func logMessages(channel slack.Channel, userName, botName, msg, response string) {
	filename := channel.Name
	if channel.IsIM {
		filename = userName
	}
	if filename == "" {
		filename = channel.ID
	}
	filename = path.Join(*logDir, filename)

	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Error opening log file for writing: %s", err)
		return
	}
	defer f.Close()
	msg = fmt.Sprintf("[%s] %s\n", userName, msg)
	if _, err := f.WriteString(msg); err != nil {
		log.Printf("Error appending message to log file: %s", err)
	}

	response = fmt.Sprintf("[%s] %s\n", botName, response)
	if _, err := f.WriteString(response); err != nil {
		log.Printf("Error appending response to log file: %s", err)
	}
}
