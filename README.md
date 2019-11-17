# Markov Bot

Fun with [Markov Chains](https://en.wikipedia.org/wiki/Markov_chain) and Slack
bots.

## Usage

```
Usage of markov-bot:
  -bot-token string
    	Slack bot token
  -buffer int
    	Buffer size (default 1000)
  -cache string
    	Cache directory (default "./cache")
  -concurrency int
    	Concurrency (default 3)
  -email string
    	Email address of slack user to create bot for
  -input-words int
    	Number of input words required to definitely choose one as the starting prefix (default 3)
  -logs string
    	Log directory (default "./logs")
  -option-min int
    	Minimum number of options before downgrading to shorter prefix (default 2)
  -prefix-max int
    	Maximum prefix length (default 5)
  -prefix-min int
    	Minimum prefix length, even if below option-min (default 2)
  -sentence-attempts int
    	Number of times to try building a sentence longer than minimum (default 5)
  -sentence-max int
    	Target maximum sentence length (default 20)
  -sentence-min int
    	Target minimum sentence length (default 5)
  -stopwords string
    	Stopwords file (default "./stopwords.txt")
  -user-token string
    	Slack user token
```

