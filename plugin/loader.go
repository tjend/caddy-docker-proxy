package plugin

import (
	"bytes"
	"context"
	"flag"
	"log"
	"os"
	"time"

	"github.com/caddyserver/caddy"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

var pollingInterval = 30 * time.Second
var processCaddyfileFlag bool

func init() {
	flag.DurationVar(&pollingInterval, "docker-polling-interval", 30*time.Second, "Interval caddy should manually check docker for a new caddyfile")
	flag.BoolVar(&processCaddyfileFlag, "docker-process-caddyfile", false, "Process caddyfile, removing invalid servers")
}

// DockerLoader generates caddy files from docker swarm information
type DockerLoader struct {
	initialized       bool
	dockerClient      *client.Client
	generator         *CaddyfileGenerator
	timer             *time.Timer
	skipEvents        bool
	input             caddy.CaddyfileInput
	processCaddyfile  bool
	previousCaddyfile []byte
	previousLogs      string
}

// CreateDockerLoader creates a docker loader
func CreateDockerLoader() *DockerLoader {
	return &DockerLoader{
		input: caddy.CaddyfileInput{
			ServerTypeName: "http",
		},
	}
}

// Load returns the current caddy file input
func (dockerLoader *DockerLoader) Load(serverType string) (caddy.Input, error) {
	if serverType != "http" {
		return nil, nil
	}
	if !dockerLoader.initialized {
		dockerLoader.initialized = true

		dockerClient, err := client.NewEnvClient()
		if err != nil {
			log.Printf("Docker connection failed: %v", err)
			return nil, nil
		}

		dockerPing, err := dockerClient.Ping(context.Background())
		if err != nil {
			log.Printf("Docker ping failed: %v", err)
			return nil, nil
		}

		dockerClient.NegotiateAPIVersionPing(dockerPing)

		dockerLoader.dockerClient = dockerClient
		dockerLoader.generator = CreateGenerator(
			WrapDockerClient(dockerClient),
			CreateDockerUtils(),
			GetGeneratorOptions(),
		)

		if processCaddyfileEnv := os.Getenv("CADDY_DOCKER_PROCESS_CADDYFILE"); processCaddyfileEnv != "" {
			dockerLoader.processCaddyfile = isTrue.MatchString(processCaddyfileEnv)
		} else {
			dockerLoader.processCaddyfile = processCaddyfileFlag
		}
		log.Printf("[INFO] Docker process caddyfile: %v", dockerLoader.processCaddyfile)

		if pollingIntervalEnv := os.Getenv("CADDY_DOCKER_POLLING_INTERVAL"); pollingIntervalEnv != "" {
			if p, err := time.ParseDuration(pollingIntervalEnv); err != nil {
				log.Printf("Failed to parse CADDY_DOCKER_POLLING_INTERVAL: %v", err)
			} else {
				pollingInterval = p
			}
		}
		log.Printf("[INFO] Docker polling interval: %v", pollingInterval)
		dockerLoader.timer = time.AfterFunc(pollingInterval, func() {
			dockerLoader.update(true)
		})

		dockerLoader.update(false)

		go dockerLoader.monitorEvents()
	}
	return dockerLoader.input, nil
}

func (dockerLoader *DockerLoader) monitorEvents() {
	args := filters.NewArgs()
	args.Add("scope", "swarm")
	args.Add("scope", "local")
	args.Add("type", "service")
	args.Add("type", "container")
	args.Add("type", "config")

	ctx := context.Background()
	cancelCtx, cancelFunc := context.WithCancel(ctx)

	eventsChan, errorChan := dockerLoader.dockerClient.Events(cancelCtx, types.EventsOptions{
		Filters: args,
	})

	forloop: // label the for loop so we can break out of it
	for {
		select {
		case event := <-eventsChan:
			if dockerLoader.skipEvents {
				continue
			}

			update := (event.Type == "container" && event.Action == "create") ||
				(event.Type == "container" && event.Action == "start") ||
				(event.Type == "container" && event.Action == "stop") ||
				(event.Type == "container" && event.Action == "die") ||
				(event.Type == "container" && event.Action == "destroy") ||
				(event.Type == "service" && event.Action == "create") ||
				(event.Type == "service" && event.Action == "update") ||
				(event.Type == "service" && event.Action == "remove") ||
				(event.Type == "config" && event.Action == "create") ||
				(event.Type == "config" && event.Action == "remove")

			if update {
				dockerLoader.skipEvents = true
				dockerLoader.timer.Reset(100 * time.Millisecond)
			}
		case err := <-errorChan:
			if err == nil {
				// docker client event error, is the docker socket no longer accessible?
				// cancel the context, otherwise the for loop errors forever
				cancelFunc()
				break forloop // break out of the for loop
			} else {
				// log unknown error
				log.Println(err)
			}

		}
	}
}

func (dockerLoader *DockerLoader) update(reloadIfChanged bool) bool {
	dockerLoader.timer.Reset(pollingInterval)
	dockerLoader.skipEvents = false

	caddyfile, logs, err := dockerLoader.generator.GenerateCaddyFile()

	// error is returned if docker swarm is down and we want to leave the caddyfile as is
	if err != nil {
		log.Printf("[INFO] ignoring docker swarm error, leaving caddyfile as is: %v\n", err.Error())
		return false
	}

	caddyfileChanged := !bytes.Equal(dockerLoader.previousCaddyfile, caddyfile)
	logsChanged := dockerLoader.previousLogs != logs
	dockerLoader.previousCaddyfile = caddyfile
	dockerLoader.previousLogs = logs

	if logsChanged || caddyfileChanged {
		log.Print(logs)
	}

	if !caddyfileChanged {
		return false
	}

	if dockerLoader.processCaddyfile {
		log.Printf("[INFO] Processing caddyfile")
		caddyfile = ProcessCaddyfile(caddyfile)
	}

	if len(caddyfile) == 0 {
		caddyfile = []byte("# Empty caddyfile")
	}

	newInput := caddy.CaddyfileInput{
		ServerTypeName: "http",
		Contents:       caddyfile,
	}

	if err := caddy.ValidateAndExecuteDirectives(newInput, nil, true); err != nil {
		log.Printf("[ERROR] CaddyFile error: %s", err)
		log.Printf("[INFO] Wrong CaddyFile:\n%s", caddyfile)
	} else {
		log.Printf("[INFO] New CaddyFile:\n%s", newInput.Contents)

		dockerLoader.input = newInput

		if reloadIfChanged {
			ReloadCaddy(dockerLoader)
		}
	}

	return true
}
