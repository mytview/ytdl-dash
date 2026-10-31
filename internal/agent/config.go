package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is everything the agent reads from the environment. Only INSTANCE is required.
type Config struct {
	Instance string // INSTANCE: the name on the card
	Host     string // HOST: the machine's name, shown on the card; default the Docker host's name through the socket, else this container's hostname

	ConfigDir       string // CONFIG_DIR, default /config: ytdl-sub's config folder (its cron log, subscriptions and .last-success)
	LogFile         string // LOG_FILE, default CONFIG_DIR/.cron.log
	SubsFile        string // SUBSCRIPTIONS, default CONFIG_DIR/subscriptions.yaml
	SubsInContainer string // SUBSCRIPTIONS_IN_CONTAINER, default /config/subscriptions.yaml: read through the socket first, since a
	// bind-mounted file is what the container runs with and may not be in CONFIG_DIR on the host; empty turns it off
	MediaDir string // MEDIA_DIR, default /media: where the channel folders and archives are
	StateDir string // STATE_DIR, default /data: the agent's own run history

	Listen string // LISTEN, default :9100
	Token  string // TOKEN: bearer token for POST commands; without it commands are off

	Container      string // YTDL_CONTAINER: the ytdl-sub container's name, for health, run, stop and version (needs the socket)
	DockerSocket   string // DOCKER_SOCKET, default /var/run/docker.sock
	RunCommand     string // RUN_COMMAND: what "run now" executes in the container
	RunUser        string // RUN_USER, default abc (LinuxServer's user)
	StopCommand    string // STOP_COMMAND: what "stop" executes
	VersionCommand string // VERSION_COMMAND, default "ytdl-sub --version"

	GluetunURL string // GLUETUN_API_URL: makes gluetun the exit provider and enables gluetun-reconnect
	GluetunKey string // GLUETUN_API_KEY
	ExitLabel  string // EXIT_LABEL, default "own address": the exit text without a provider

	Rescan       time.Duration // RESCAN, default 30m: archive scan while idle (always after a run)
	HealthMaxAge time.Duration // HEALTH_MAX_AGE, default 6h: without a container healthcheck, how old .last-success may be
	Version      string
}

// ConfigFromEnv reads the environment.
func ConfigFromEnv(version string) (Config, error) {
	c := Config{Version: version}
	env := func(k, def string) string {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
		return def
	}
	c.Instance = env("INSTANCE", "")
	if c.Instance == "" {
		return c, fmt.Errorf("INSTANCE is required: the name of this ytdl-sub instance")
	}
	c.Host = env("HOST", "") // resolved in New: the daemon's name when the socket answers, else os.Hostname
	c.ConfigDir = env("CONFIG_DIR", "/config")
	c.LogFile = env("LOG_FILE", filepath.Join(c.ConfigDir, ".cron.log"))
	c.SubsFile = env("SUBSCRIPTIONS", filepath.Join(c.ConfigDir, "subscriptions.yaml"))
	c.SubsInContainer = env("SUBSCRIPTIONS_IN_CONTAINER", "/config/subscriptions.yaml")
	if c.SubsInContainer == "none" {
		c.SubsInContainer = ""
	}
	c.MediaDir = env("MEDIA_DIR", "/media")
	c.StateDir = env("STATE_DIR", "/data")
	c.Listen = env("LISTEN", ":9100")
	c.Token = env("TOKEN", "")
	c.Container = env("YTDL_CONTAINER", "")
	c.DockerSocket = env("DOCKER_SOCKET", "/var/run/docker.sock")
	c.RunCommand = env("RUN_COMMAND", `sh -c 'bash /config/cron "$@" >> /config/.cron.log 2>&1' cron`) // a check appends --match NAME
	c.RunUser = env("RUN_USER", "abc")
	c.StopCommand = env("STOP_COMMAND", "pkill -TERM -f [y]tdl-sub\\ -l")
	c.VersionCommand = env("VERSION_COMMAND", "ytdl-sub --version")
	c.GluetunURL = strings.TrimRight(env("GLUETUN_API_URL", ""), "/")
	c.GluetunKey = env("GLUETUN_API_KEY", "")
	c.ExitLabel = env("EXIT_LABEL", "own address")
	var err error
	if c.Rescan, err = time.ParseDuration(env("RESCAN", "30m")); err != nil {
		return c, fmt.Errorf("RESCAN: %w", err)
	}
	if c.HealthMaxAge, err = time.ParseDuration(env("HEALTH_MAX_AGE", "6h")); err != nil {
		return c, fmt.Errorf("HEALTH_MAX_AGE: %w", err)
	}
	return c, nil
}
