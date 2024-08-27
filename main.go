package main

import (
	"github.com/gitu/gitlab-util/pkg/ggl"
	"github.com/gitu/gitlab-util/pkg/glui"
	"github.com/urfave/cli/v2"
	"log"
	"log/slog"
	"os"
)

var (
	version = "dev"
	commit  = "none"
)

func main() {

	cli.VersionPrinter = func(cCtx *cli.Context) {
		slog.Info("gitlab-util",
			"version", version, "commit", commit,
			"compiledAt", cCtx.App.Compiled.Format("2006-01-02 15:04:05"))
	}
	cli.VersionFlag = &cli.BoolFlag{
		Name:    "print-version",
		Aliases: []string{"V"},
		Usage:   "print only the version",
	}

	app := cli.NewApp()

	app.Name = "gitlab-util"
	app.Usage = "server to fight with your family in a more or less healthy way"
	app.Version = version

	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:    "gitlab-url",
			Usage:   "gitlab url to connect to (e.g. https://gitlab.yourdomain.com/api/v4)  (can be set via GITLAB_URL env var if not used last logged in url is used)",
			EnvVars: []string{"GITLAB_URL"},
		},
	}

	app.Commands = []*cli.Command{
		{
			Name:  "login",
			Usage: "login to gitlab",
			Flags: []cli.Flag{
				&cli.StringFlag{
					Name:     "token",
					Aliases:  []string{"gitlab-token", "t"},
					Usage:    "gitlab token",
					EnvVars:  []string{"GITLAB_TOKEN"},
					Required: true,
				},
				&cli.StringFlag{
					Name:     "url",
					Aliases:  []string{"gitlab-url", "u"},
					Usage:    "gitlab url to connect to (e.g. https://gitlab.yourdomain.com/api/v4) for login (required, can be set via GITLAB_URL env var if not used last logged in url is used)",
					EnvVars:  []string{"GITLAB_URL"},
					Required: true,
				},
			},
			Action: func(c *cli.Context) error {
				return ggl.Login(c.String("token"), c.String("url"))
			},
		},
		{
			Name:  "auto-merge",
			Usage: "automatically approves and tries to merge merge requeusts of a user (renovate bot)",
			Flags: []cli.Flag{
				&cli.StringFlag{
					Name:  "author",
					Usage: "author of the merge requests to auto merge (e.g. renovate-bot)",
				},
				&cli.StringFlag{
					Name:  "reviewer",
					Usage: "reviewer of the merge requests to auto merge (e.g. your username)",
				},
				&cli.StringFlag{
					Name:  "log-file",
					Usage: "log file to write log into - optional",
				},
			},
			Action: func(c *cli.Context) error {
				if c.String("author") == "" && c.String("reviewer") == "" {
					return cli.ShowCommandHelp(c, "")
				}
				return glui.AutoMerge(c.String("author"), c.String("reviewer"), c.String("log-file"))
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}
