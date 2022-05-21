// This program deletes old messages and files in your Slack workspace.
package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/spf13/pflag"
)

func main() {
	// Slack OAuth Access Token beginning with `xoxp-`.
	// To get a token you'll need to create a new Slack app and add
	// it to your workspace:
	// https://api.slack.com/apps
	//
	// You'll also need to give your app the correct scopes (permissions):
	//
	// channels:history - View messages and other content in a user’s public channels
	// channels:read - View basic information about public channels in a workspace
	// chat:write - Send messages on a user’s behalf
	// files:read - View files shared in channels and conversations that a user has access to
	// files:write - Upload, edit, and delete files on a user’s behalf
	// groups:history - View messages and other content in a user’s private channels
	// groups:read - View basic information about a user’s private channels
	// im:history - View messages and other content in a user’s direct messages
	// im:read - View basic information about a user’s direct messages
	// mpim:history - View messages and other content in a user’s group direct messages
	// mpim:read - View basic information about a user’s group direct messages
	// users:read - View people in a workspace
	//
	token := pflag.String("token", "", "Slack token")

	// Limited cleanup to messages and files owned by a specific Slack user.
	user := pflag.String("user", "", "Limit cleanup to a specific Slack user (username without the `@` sign)")

	// Filter public, private, multiparty and DM channels using this pattern.
	// This limits cleanup to channels that match the filter.
	filter := pflag.String("filter", "", "Filter channels (public, private, multi-party and DM) using this pattern, e.g. 'foo,bar' becomes regexp /(foo|bar)/i")

	// Delete messages and files before this date.
	before := pflag.String("before", "", "Delete messages and files before this date (REQUIRED, format: YYYYMMDD-HHII)")

	commit := pflag.Bool("commit", false, "Perform the actual delete operations. Omitting this flag will perform a DRY-RUN.")

	pflag.Parse()

	// Try loading token from env var if no --token flag was passed.
	if *token == "" {
		*token = os.Getenv("MY_SLACK_TOKEN")
		if *token == "" {
			panic("--token flag (or MY_SLACK_TOKEN env var) required but missing")
		}
	}

	if *before == "" {
		pflag.PrintDefaults()
		panic("--before flag is required but missing")
	}

	s := New(SlackCleanOptions{
		Before: *before,
		Token:  *token,
	})

	// Get all users in workspace.
	users, userID := s.Users(*user)

	// Get channels.
	channels := s.Channels(*filter, users)

	// Get all files.
	files := s.Files(userID, channels)

	// Get messages.
	messages := s.Messages(channels, userID)

	fmt.Printf("\nFound %d messages and %d files to delete!\n\n", len(messages), len(files))

	if !*commit {
		fmt.Printf("Run command again with --commit flag to perform the delete operations!\n\n")
		os.Exit(0)
	}

	// Delete messages.
	s.DeleteMessages(messages)

	// Delete files.
	s.DeleteFiles(files)

	fmt.Printf("\nIt's a Done Deal!\n\n")
}

type SlackClean struct {
	c            *slack.Client
	beforeTS     string
	beforeTSUnix slack.JSONTime
}

type SlackCleanOptions struct {
	Before string // e.g. 20060102-1504
	Token  string // e.g  xoxp-...
}

func New(o SlackCleanOptions) (s *SlackClean) {
	s = new(SlackClean)

	t, err := time.Parse("20060102-1504", o.Before)
	if err != nil {
		panic(err)
	}

	s.beforeTS = s.TimeToSlackTS(t)
	s.beforeTSUnix = slack.JSONTime(t.Unix())

	fmt.Printf("Cleaning Slack messages before: %s (Slack TS: %s , Check: %s)\n", prettyDate(t), s.beforeTS, prettyDate(s.beforeTSUnix.Time()))

	fmt.Printf("Using token: %s\n", o.Token)

	s.c = slack.New(o.Token)

	return
}

func (s *SlackClean) SlackTSToTime(ts string) time.Time {
	parts := strings.Split(ts, ".")
	sec, _ := strconv.ParseInt(parts[0], 10, 64)
	nsec, _ := strconv.ParseInt(parts[1], 10, 64)
	return time.Unix(sec, nsec*1000)
}

func (s *SlackClean) TimeToSlackTS(t time.Time) string {
	ts := fmt.Sprintf("%d", t.UnixNano()/1000)
	return fmt.Sprintf("%s.%s", ts[0:len(ts)-6], ts[len(ts)-6:])
}

func (s *SlackClean) ratelimitOrPanic(err error) {
	if !strings.Contains(err.Error(), "rate limit") {
		panic(err)
	}

	fmt.Printf("Rate limited, retrying in 1 sec ..\n")
	time.Sleep(1000 * time.Millisecond)
}

func (s *SlackClean) Users(user string) (users map[string]string, userID string) {
	users = make(map[string]string)

	res, err := s.c.GetUsers()
	if err != nil {
		panic(err)
	}

	for _, u := range res {
		if user != "" {
			if u.Name == user {
				userID = u.ID
			} else {
				fmt.Printf("Checked user %s (real name: %s)\n", u.Name, u.RealName)
			}
		}
		users[u.ID] = u.Name
	}

	if user != "" {
		if userID == "" {
			panic("could not find user: " + user)
		}
		fmt.Printf("Found user %s (ID: %s)\n", users[userID], userID)
	}

	fmt.Printf("Fetched %d users\n", len(users))

	return
}

func (s *SlackClean) Files(userID string, channels []slack.Channel) (filesToDelete []slack.File) {
	var found int

	for _, c := range channels {
		p := &slack.ListFilesParameters{
			Limit:   100,
			Channel: c.ID,
		}

		if userID != "" {
			p.User = userID
			fmt.Printf("Fetching only files owned by user ID %s in channel %s (name: %s)\n", userID, c.ID, c.Name)
		}

		for {
			var res []slack.File
			var err error

			res, p, err := s.c.ListFiles(*p)
			if err != nil {
				s.ratelimitOrPanic(err)
			}

			for _, f := range res {
				if f.Created < s.beforeTSUnix {
					found++
					fmt.Printf("%04d. Found file %s (created: %s)\n", found, f.Name, prettyDate(f.Created.Time()))
					filesToDelete = append(filesToDelete, f)
				}
			}

			if p.Cursor == "" {
				break
			}

			fmt.Printf("Fetching more files (cursor: %s , files: %d)\n", p.Cursor, len(filesToDelete))
		}
	}

	fmt.Printf("Fetched %d files\n", len(filesToDelete))

	return
}

func (s *SlackClean) Channels(filter string, users map[string]string) (channels []slack.Channel) {
	var found int
	var cursor string

	// Create filter regexp.
	var re *regexp.Regexp
	if filter != "" {
		re = regexp.MustCompile(`(?i)(` + strings.ReplaceAll(filter, ",", "|") + `)`)
		fmt.Printf("Filtering channels using regexp pattern: %s\n", re.String())
	}

	for {
		res, next, err := s.c.GetConversations(&slack.GetConversationsParameters{
			Types:  []string{"public_channel", "private_channel", "mpim", "im"},
			Cursor: cursor,
		})
		if err != nil {
			s.ratelimitOrPanic(err)
			continue
		}

		for _, c := range res {
			if c.IsMpIM {
				// Include all MpIM channels if there's no filter.
				if re == nil || re.MatchString(c.Name) {
					found++
					fmt.Printf("%04d. Found multiparty DM channel ID %s (name: %s)\n", found, c.ID, c.Name)
					channels = append(channels, c)
				}
			} else if c.IsIM {
				username := users[c.User]
				// Include all DM channels if there's no filter.
				if re == nil || re.MatchString(username) {
					found++
					fmt.Printf("%04d. Found DM channel ID %s (name: %s)\n", found, c.ID, username)
					channels = append(channels, c)
				}
			} else if c.IsPrivate {
				if re == nil || re.MatchString(c.Name) {
					found++
					fmt.Printf("%04d. Found private channel ID %s (name: %s)\n", found, c.ID, c.Name)
					channels = append(channels, c)
				}
			} else {
				if re == nil || re.MatchString(c.Name) {
					found++
					fmt.Printf("%04d. Found public channel ID %s (name: %s)\n", found, c.ID, c.Name)
					channels = append(channels, c)
				}
			}
		}

		if next == "" {
			break
		}

		fmt.Printf("Fetching more channels (cursor: %s , channels: %d)\n", next, len(channels))
		cursor = next
	}

	fmt.Printf("Fetched %d channels\n", len(channels))

	return
}

func (s *SlackClean) Messages(channels []slack.Channel, userID string) (messagesToDelete []slack.Message) {
	var found int
	var keep int
	var total int

	clean := regexp.MustCompile(`[\r\n\t]+`)

	for i, c := range channels {
		fmt.Printf("%04d. Fetching messages for channel ID %s (name: %s)\n", i+1, c.ID, c.Name)

		var cursor string
		var last int

		for {
			res, err := s.c.GetConversationHistory(&slack.GetConversationHistoryParameters{
				ChannelID: c.ID,
				Cursor:    cursor,
				Limit:     1000,
			})
			if err != nil {
				s.ratelimitOrPanic(err)
				continue
			}

			for _, m := range res.Messages {
				total++

				if userID != "" {
					if m.User == userID {
						keep++
						continue
					}
				}

				if m.Timestamp > s.beforeTS {
					keep++
					continue
				}

				if found == last {
					fmt.Println("")
				}

				found++

				channelName := c.Name
				if channelName == "" {
					channelName = c.ID
				}

				preview := clean.ReplaceAllString(m.Text, " ")
				if len(preview) > 50 {
					preview = preview[0:46] + " ..."
				}
				fmt.Printf("%04d. [%-30s] [ts: %s]  --  %s\n", found, "channel : "+channelName, prettyDate(s.SlackTSToTime(m.Timestamp)), preview)

				m.Channel = c.ID
				messagesToDelete = append(messagesToDelete, m)

				if m.ThreadTimestamp != "" && m.Timestamp == m.ThreadTimestamp {
					// Found parent message of a thread.
					// Fetch replies and delete those too.
					p := &slack.GetConversationRepliesParameters{
						ChannelID: m.Channel,
						Timestamp: m.Timestamp,
						Limit:     1000,
					}
					for {
						replies, hasMore, nextCursor, err := s.c.GetConversationReplies(p)
						if err != nil {
							s.ratelimitOrPanic(err)
							continue
						}

						for _, r := range replies {
							r.Channel = c.ID
							messagesToDelete = append(messagesToDelete, r)

							preview := clean.ReplaceAllString(r.Text, " ")
							if len(preview) > 50 {
								preview = preview[0:46] + " ..."
							}
							fmt.Printf("%04d. [%-30s] [ts: %s]  --  %s\n", found, "thread  : "+channelName, prettyDate(s.SlackTSToTime(r.Timestamp)), preview)

							total++
							found++
						}

						if !hasMore {
							break
						}

						p.Cursor = nextCursor
					}
				}
			}

			if !res.HasMore {
				break
			}

			cursor = res.ResponseMetaData.NextCursor

			if found == last {
				fmt.Printf(".")
			} else {
				fmt.Printf("Fetching more messages for channel ID %s (cursor: %s , messages: %d)\n", c.ID, cursor, len(messagesToDelete))
			}
			last = found
		}
	}

	fmt.Printf("Fetched %d messages to delete (kept %d / %d)\n", len(messagesToDelete), keep, total)

	return
}

func (s *SlackClean) DeleteMessages(messages []slack.Message) {
	for i, m := range messages {

		for {
			ch, ts, err := s.c.DeleteMessage(m.Channel, m.Timestamp)
			if err != nil {
				if err.Error() == "message_not_found" {
					break
				}
				s.ratelimitOrPanic(err)
				continue
			}

			fmt.Printf("%04d. Deleted message ID %s in channel ID %s\n", i+1, ts, ch)
			break
		}

		time.Sleep(50 * time.Millisecond)
	}
}

func (s *SlackClean) DeleteFiles(files []slack.File) {
	for i, f := range files {
		for {
			err := s.c.DeleteFile(f.ID)
			if err != nil {
				if err.Error() == "file_not_found" {
					break
				}
				s.ratelimitOrPanic(err)
				continue
			}

			fmt.Printf("%04d. Deleted file named %s (created: %s)\n", i+1, f.Name, prettyDate(f.Created.Time()))
			break
		}

		time.Sleep(50 * time.Millisecond)
	}
}

func prettyDate(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
