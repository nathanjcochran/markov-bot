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
  -option-min int
    	Minimum number of options before downgrading to shorter prefix (default 5)
  -prefix-length int
    	Prefix length (default 4)
  -prefix-min int
    	Minimum prefix length, even if below option-min (default 2)
  -sentence-attempts int
    	Number of times to try building a sentence longer than minimum (default 5)
  -sentence-length int
    	Minimum sentence length, unless there are no other options (default 5)
  -user-token string
    	Slack user token
```

