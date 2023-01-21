package cmd

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/arimanius/digivision-backend/internal/bootstrap"
	"github.com/arimanius/digivision-backend/internal/bootstrap/job"

	"github.com/arimanius/digivision-backend/internal/cfg"
	"github.com/arimanius/digivision-backend/internal/jobs"
	"github.com/arimanius/digivision-backend/internal/server"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func addServeCmd(root *cobra.Command) {
	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "GRPC Server",
		Run: func(cmd *cobra.Command, args []string) {
			serve(cmd)
		},
	}

	root.AddCommand(serveCmd)
	serveCmd.Flags().StringP("config", "c", "", "Config file path")
	serveCmd.Flags().BoolP("dev", "d", false, "Run with development config")
}

func serve(cmd *cobra.Command) {
	config := loadConfig(cmd)
	bootstrap.AdjustLogLevel(config.Log.Level)

	var terminableJobs []job.WithGracefulShutDown
	terminableJobs = append(terminableJobs, server.RunServer(config))
	terminableJobs = append(terminableJobs, jobs.StartJobs(config)...)

	terminateOnSignals(terminableJobs)
}

func loadConfig(cmd *cobra.Command) cfg.Config {
	configPath, _ := cmd.Flags().GetString("config")
	if configPath != "" {
		return cfg.ParseConfig(configPath)
	}
	if dev, _ := cmd.Flags().GetBool("dev"); dev {
		logrus.Infof("No config file specified. Falling back to dev config.")
		return cfg.ParseDevConfig()
	}
	logrus.Info("No config file specified. Only env variables will be used as config.")
	return cfg.ParseConfig("")
}

func terminateOnSignals(terminableJobs []job.WithGracefulShutDown) {
	c := make(chan os.Signal, 2)
	signal.Notify(c, syscall.SIGINT)
	signal.Notify(c, syscall.SIGTERM)
	sig := <-c
	logrus.Infof("Received sig-%s.\n", sig.String())

	if err := job.ShutDown(terminableJobs, 5*time.Second); err != nil {
		logrus.Error(err.Error())
	}
}