package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/catatsuy/notify_slack/config"
	"github.com/catatsuy/notify_slack/slack"
	"github.com/catatsuy/notify_slack/throttle"
	"github.com/pkg/errors"
)

const (
	Version = "v0.2.1"

	ExitCodeOK             = 0
	ExitCodeParseFlagError = 1
	ExitCodeFail           = 1
)

type CLI struct {
	outStream, errStream io.Writer
	inputStream          io.Reader

	sClient *slack.Client
	conf    *config.Config
}

func NewCLI(outStream, errStream io.Writer, inputStream io.Reader) *CLI {
	return &CLI{outStream: outStream, errStream: errStream, inputStream: inputStream}
}

func (c *CLI) Run(args []string) int {
	var (
		version        bool
		tomlFile       string
		uploadFilename string
		filetype       string
	)

	c.conf = config.NewConfig()

	flags := flag.NewFlagSet("notify_slack", flag.ContinueOnError)
	flags.SetOutput(c.errStream)

	flags.StringVar(&c.conf.PrimaryChannel, "channel", "", "specify channel")
	flags.StringVar(&c.conf.SlackURL, "slack-url", "", "slack url")
	flags.StringVar(&c.conf.Token, "token", "", "token (for uploading to snippet)")
	flags.StringVar(&c.conf.Username, "username", "", "specify username")
	flags.StringVar(&c.conf.IconEmoji, "icon-emoji", "", "specify icon emoji")
	flags.DurationVar(&c.conf.Duration, "interval", time.Second, "interval")
	flags.StringVar(&tomlFile, "c", "", "config file name")
	flags.StringVar(&uploadFilename, "filename", "", "specify a file name (for uploading to snippet)")
	flags.StringVar(&filetype, "filetype", "", "specify a filetype (for uploading to snippet)")

	flags.BoolVar(&version, "version", false, "Print version information and quit")

	err := flags.Parse(args[1:])
	if err != nil {
		return ExitCodeParseFlagError
	}

	if version {
		fmt.Fprintf(c.errStream, "notify_slack version %s\n", Version)
		return ExitCodeOK
	}

	argv := flags.Args()
	filename := ""
	if len(argv) == 1 {
		filename = argv[0]
	}

	tomlFile = config.LoadTOMLFilename(tomlFile)

	if tomlFile != "" {
		err := c.conf.LoadTOML(tomlFile)
		if err != nil {
			fmt.Fprintln(c.errStream, err)
			return ExitCodeFail
		}
	}

	if c.conf.SlackURL == "" {
		fmt.Fprintln(c.errStream, "must specify Slack URL")
		return ExitCodeFail
	}

	c.sClient, err = slack.NewClient(c.conf.SlackURL, nil)
	if err != nil {
		fmt.Fprintln(c.errStream, err)
		return ExitCodeFail
	}

	if filename != "" {
		if c.conf.Token == "" {
			fmt.Fprintln(c.errStream, "must specify Slack token for uploading to snippet")
			return ExitCodeFail
		}

		err := c.uploadSnippet(context.Background(), filename, uploadFilename, filetype)
		if err != nil {
			fmt.Fprintln(c.errStream, err)
			return ExitCodeFail
		}

		return ExitCodeOK
	}

	copyStdin := io.TeeReader(c.inputStream, c.outStream)

	ex := throttle.NewExec(copyStdin)

	exitC := make(chan os.Signal, 0)
	signal.Notify(exitC, syscall.SIGTERM, syscall.SIGINT)

	channel := c.conf.PrimaryChannel
	if channel == "" {
		channel = c.conf.Channel
	}

	param := &slack.PostTextParam{
		Channel:   channel,
		Username:  c.conf.Username,
		IconEmoji: c.conf.IconEmoji,
	}

	flushCallback := func(_ context.Context, output string) error {
		param.Text = output
		return c.sClient.PostText(context.Background(), param)
	}

	done := make(chan struct{}, 0)

	doneCallback := func(ctx context.Context, output string) error {
		defer func() {
			done <- struct{}{}
		}()

		return flushCallback(ctx, output)
	}

	interval := time.Tick(c.conf.Duration)
	ctx, cancel := context.WithCancel(context.Background())

	ex.Start(ctx, interval, flushCallback, doneCallback)

	select {
	case <-exitC:
	case <-ex.Wait():
	}
	cancel()

	<-done

	return ExitCodeOK
}

func (c *CLI) uploadSnippet(ctx context.Context, filename, uploadFilename, filetype string) error {
	channel := c.conf.PrimaryChannel
	if channel == "" {
		channel = c.conf.SnippetChannel
	}
	if channel == "" {
		channel = c.conf.Channel
	}

	if channel == "" {
		return fmt.Errorf("must specify channel for uploading to snippet")
	}

	_, err := os.Stat(filename)
	if err != nil {
		return errors.Wrapf(err, "%s does not exist", filename)
	}

	content, err := ioutil.ReadFile(filename)
	if err != nil {
		return err
	}

	if uploadFilename == "" {
		uploadFilename = filename
	}

	param := &slack.PostFileParam{
		Channel:  channel,
		Filename: uploadFilename,
		Content:  string(content),
		Filetype: filetype,
	}
	err = c.sClient.PostFile(ctx, c.conf.Token, param)
	if err != nil {
		return err
	}

	return nil
}
