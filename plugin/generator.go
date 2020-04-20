package plugin

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"math"
	"math/big"
	rand "math/rand"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
)

var swarmAvailabilityCacheInterval = 1 * time.Minute

var defaultLabelPrefix = "caddy"

// CaddyfileGenerator generates caddyfile
type CaddyfileGenerator struct {
	caddyFilePath        string
	labelPrefix          string
	labelRegex           *regexp.Regexp
	ignoreSwarmError     bool
	proxyServiceTasks    bool
	validateNetwork      bool
	dockerClient         DockerClient
	dockerUtils          DockerUtils
	caddyNetworks        map[string]bool
	swarmIsAvailable     bool
	swarmIsAvailableTime time.Time
}

var isTrue = regexp.MustCompile("(?i)^(true|yes|1)$")
var suffixRegex = regexp.MustCompile("_\\d+$")

var labelPrefixFlag string
var caddyFilePath string
var ignoreSwarmErrorFlag bool
var proxyServiceTasksFlag bool
var validateNetworkFlag bool

func init() {
	flag.StringVar(&labelPrefixFlag, "docker-label-prefix", defaultLabelPrefix, "Prefix for Docker labels")
	flag.StringVar(&caddyFilePath, "docker-caddyfile-path", "", "Path to a default CaddyFile")
	flag.BoolVar(&ignoreSwarmErrorFlag, "docker-ignore-swarm-error", false, "Skip updating caddyfile if swarm is unavailable")
	flag.BoolVar(&proxyServiceTasksFlag, "proxy-service-tasks", false, "Proxy to service tasks instead of service load balancer")
	flag.BoolVar(&validateNetworkFlag, "docker-validate-network", true, "Validates if caddy container and target are in same network")
}

// GeneratorOptions are the options for generator
type GeneratorOptions struct {
	caddyFilePath     string
	labelPrefix       string
	ignoreSwarmError  bool
	proxyServiceTasks bool
	validateNetwork   bool
}

// GetGeneratorOptions creates generator options from cli flags and environment variables
func GetGeneratorOptions() *GeneratorOptions {
	options := GeneratorOptions{}

	if caddyFilePathEnv := os.Getenv("CADDY_DOCKER_CADDYFILE_PATH"); caddyFilePathEnv != "" {
		options.caddyFilePath = caddyFilePathEnv
	} else {
		options.caddyFilePath = caddyFilePath
	}

	if ignoreSwarmErrorEnv := os.Getenv("CADDY_DOCKER_IGNORE_SWARM_ERROR"); ignoreSwarmErrorEnv != "" {
		options.ignoreSwarmError = isTrue.MatchString(ignoreSwarmErrorEnv)
	} else {
		options.ignoreSwarmError = ignoreSwarmErrorFlag
	}

	if labelPrefixEnv := os.Getenv("CADDY_DOCKER_LABEL_PREFIX"); labelPrefixEnv != "" {
		options.labelPrefix = labelPrefixEnv
	} else {
		options.labelPrefix = labelPrefixFlag
	}

	if proxyServiceTasksEnv := os.Getenv("CADDY_DOCKER_PROXY_SERVICE_TASKS"); proxyServiceTasksEnv != "" {
		options.proxyServiceTasks = isTrue.MatchString(proxyServiceTasksEnv)
	} else {
		options.proxyServiceTasks = proxyServiceTasksFlag
	}

	if validateNetworkEnv := os.Getenv("CADDY_DOCKER_VALIDATE_NETWORK"); validateNetworkEnv != "" {
		options.validateNetwork = isTrue.MatchString(validateNetworkEnv)
	} else {
		options.validateNetwork = validateNetworkFlag
	}

	return &options
}

// CreateGenerator creates a new generator
func CreateGenerator(dockerClient DockerClient, dockerUtils DockerUtils, options *GeneratorOptions) *CaddyfileGenerator {
	var labelRegexString = fmt.Sprintf("^%s(_\\d+)?(\\.|$)", options.labelPrefix)

	return &CaddyfileGenerator{
		caddyFilePath:     options.caddyFilePath,
		dockerClient:      dockerClient,
		dockerUtils:       dockerUtils,
		labelPrefix:       options.labelPrefix,
		labelRegex:        regexp.MustCompile(labelRegexString),
		ignoreSwarmError:  options.ignoreSwarmError,
		proxyServiceTasks: options.proxyServiceTasks,
		validateNetwork:   options.validateNetwork,
	}
}

// GenerateCaddyFile generates a caddy file config from docker swarm
func (g *CaddyfileGenerator) GenerateCaddyFile() ([]byte, string, error) {
	var caddyfileBuffer bytes.Buffer
	var logsBuffer bytes.Buffer

	if g.validateNetwork && g.caddyNetworks == nil {
		networks, err := g.getCaddyNetworks()
		if err == nil {
			g.caddyNetworks = map[string]bool{}
			for _, network := range networks {
				g.caddyNetworks[network] = true
			}
		} else {
			logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
		}
	}

	if time.Since(g.swarmIsAvailableTime) > swarmAvailabilityCacheInterval {
		g.checkSwarmAvailability(time.Time.IsZero(g.swarmIsAvailableTime))
		g.swarmIsAvailableTime = time.Now()
	}

	if g.ignoreSwarmError && !g.swarmIsAvailable {
		// return error to skip updating caddyfile
		return nil, logsBuffer.String(), fmt.Errorf("swarm is unavailable")
	}

	directives := map[string]*directiveData{}

	if g.caddyFilePath != "" {
		dat, err := ioutil.ReadFile(g.caddyFilePath)

		if err == nil {
			_, err = caddyfileBuffer.Write(dat)
		}

		if err != nil {
			logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
		}
	} else {
		logsBuffer.WriteString("[INFO] Skipping default CaddyFile because no path is set\n")
	}

	containers, err := g.dockerClient.ContainerList(context.Background(), types.ContainerListOptions{})
	if err == nil {
		for _, container := range containers {
			containerDirectives, err := g.getContainerDirectives(&container)
			if err == nil {
				for k, directive := range containerDirectives {
					directives[k] = mergeDirectives(directives[k], directive)
				}
			} else {
				logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
			}
		}
	} else {
		logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
	}

	if g.swarmIsAvailable {
		services, err := g.dockerClient.ServiceList(context.Background(), types.ServiceListOptions{})
		if err == nil {
			for _, service := range services {
				serviceDirectives, err := g.getServiceDirectives(&service)
				if err == nil {
					for k, directive := range serviceDirectives {
						directives[k] = mergeDirectives(directives[k], directive)
					}
				} else {
					logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
					if g.ignoreSwarmError {
						// return error to skip updating caddyfile
						return nil, logsBuffer.String(), fmt.Errorf("swarm is unavailable for getServiceDirectives")
					}
				}
			}
		} else {
			logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
			if g.ignoreSwarmError {
				// return error to skip updating caddyfile
				return nil, logsBuffer.String(), fmt.Errorf("swarm is unavailable for ServiceList")
			}
		}
	} else {
		logsBuffer.WriteString("[INFO] Skipping services because swarm is not available\n")
	}

	if g.swarmIsAvailable {
		configs, err := g.dockerClient.ConfigList(context.Background(), types.ConfigListOptions{})
		if err == nil {
			for _, config := range configs {
				if _, hasLabel := config.Spec.Labels[g.labelPrefix]; hasLabel {
					fullConfig, _, err := g.dockerClient.ConfigInspectWithRaw(context.Background(), config.ID)
					if err == nil {
						caddyfileBuffer.Write(fullConfig.Spec.Data)
						caddyfileBuffer.WriteRune('\n')
					} else {
						logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
						if g.ignoreSwarmError {
							// return error to skip updating caddyfile
							return nil, logsBuffer.String(), fmt.Errorf("swarm is unavailable for ConfigInspectWithRaw")
						}
					}
				}
			}
		} else {
			logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
			if g.ignoreSwarmError {
				// return error to skip updating caddyfile
				return nil, logsBuffer.String(), fmt.Errorf("swarm is unavailable for ConfigList")
			}
		}
	} else {
		logsBuffer.WriteString("[INFO] Skipping configs because swarm is not available\n")
	}

	writeDirectives(&caddyfileBuffer, directives, 0)

	return caddyfileBuffer.Bytes(), logsBuffer.String(), nil
}

func (g *CaddyfileGenerator) checkSwarmAvailability(isFirstCheck bool) {
	info, err := g.dockerClient.Info(context.Background())
	if err == nil {
		newSwarmIsAvailable := info.Swarm.LocalNodeState == swarm.LocalNodeStateActive
		if isFirstCheck || newSwarmIsAvailable != g.swarmIsAvailable {
			log.Printf("[INFO] Swarm is available: %v\n", newSwarmIsAvailable)
		}
		g.swarmIsAvailable = newSwarmIsAvailable
	} else {
		log.Printf("[ERROR] Swarm availability check failed: %v\n", err.Error())
		g.swarmIsAvailable = false
	}
}

func (g *CaddyfileGenerator) getCaddyNetworks() ([]string, error) {
	containerID, err := g.dockerUtils.GetCurrentContainerID()
	if err != nil {
		return nil, err
	}
	log.Printf("[INFO] Caddy ContainerID: %v\n", containerID)
	container, err := g.dockerClient.ContainerInspect(context.Background(), containerID)
	if err != nil {
		return nil, err
	}

	var networks []string
	for _, network := range container.NetworkSettings.Networks {
		networkInfo, err := g.dockerClient.NetworkInspect(context.Background(), network.NetworkID, types.NetworkInspectOptions{})
		if err != nil {
			return nil, err
		}
		if !networkInfo.Ingress {
			networks = append(networks, network.NetworkID)
		}
	}
	log.Printf("[INFO] Caddy Networks: %v\n", networks)

	return networks, nil
}

func (g *CaddyfileGenerator) parseDirectives(labels map[string]string, templateData interface{}, getProxyTargets func() ([]string, error)) (map[string]*directiveData, error) {
	originalMap := g.convertLabelsToDirectives(labels, templateData)

	convertedMap := map[string]*directiveData{}

	//Convert basic labels
	for _, directive := range originalMap {
		address := directive.children["address"]

		if address != nil && len(address.args) > 0 {
			directive.args = address.args

			sourcePath := directive.children["sourcepath"]
			targetPort := directive.children["targetport"]
			targetPath := directive.children["targetpath"]
			targetProtocol := directive.children["targetprotocol"]

			proxyDirective := getOrCreateDirective(directive.children, "proxy", false)

			if len(proxyDirective.args) == 0 {
				proxyTargets, err := getProxyTargets()
				if err != nil {
					return nil, err
				}

				if sourcePath != nil && len(sourcePath.args) > 0 {
					proxyDirective.addArgs(sourcePath.args[0])
				} else {
					proxyDirective.addArgs("/")
				}

				for _, target := range proxyTargets {
					targetArg := ""
					if targetProtocol != nil && len(targetProtocol.args) > 0 {
						targetArg += targetProtocol.args[0] + "://"
					}

					targetArg += target

					if targetPort != nil && len(targetPort.args) > 0 {
						targetArg += ":" + targetPort.args[0]
					}
					if targetPath != nil && len(targetPath.args) > 0 {
						targetArg += targetPath.args[0]
					}

					proxyDirective.addArgs(targetArg)
				}
			}
		}

		delete(directive.children, "address")
		delete(directive.children, "sourcepath")
		delete(directive.children, "targetport")
		delete(directive.children, "targetpath")
		delete(directive.children, "targetprotocol")

		//Move sites directive to main
		directive.name = strings.Join(directive.args, " ")
		directive.args = []string{}

		convertedMap[directive.name] = directive
	}

	return convertedMap, nil
}

func getOrCreateDirective(directiveMap map[string]*directiveData, path string, skipFirstDirectiveName bool) (directive *directiveData) {
	currentMap := directiveMap
	for i, p := range strings.Split(path, ".") {
		if d, ok := currentMap[p]; ok {
			directive = d
			currentMap = d.children
		} else {
			directive = &directiveData{
				children: map[string]*directiveData{},
			}
			if !skipFirstDirectiveName || i > 0 {
				directive.name = removeSuffix(p)
			}
			currentMap[p] = directive
			currentMap = directive.children
		}
	}
	return
}

func (g *CaddyfileGenerator) convertLabelsToDirectives(labels map[string]string, templateData interface{}) map[string]*directiveData {
	directiveMap := map[string]*directiveData{}

	for label, value := range labels {
		if !g.labelRegex.MatchString(label) {
			continue
		}
		directive := getOrCreateDirective(directiveMap, label, true)
		argsText := processVariables(templateData, value)
		directive.args = parseArgs(argsText)
	}

	return directiveMap
}

func processVariables(data interface{}, content string) string {
	t, err := template.New("").Parse(content)
	if err != nil {
		log.Println(err)
		return content
	}
	var writer bytes.Buffer
	t.Execute(&writer, data)
	return writer.String()
}

func parseArgs(text string) []string {
	args := regSplit(text, "\\s+")
	if len(args) == 1 && args[0] == "" {
		return []string{}
	}
	return args
}

func regSplit(text string, delimeter string) []string {
	reg := regexp.MustCompile(delimeter)
	indexes := reg.FindAllStringIndex(text, -1)
	laststart := 0
	result := make([]string, len(indexes)+1)
	for i, element := range indexes {
		result[i] = text[laststart:element[0]]
		laststart = element[1]
	}
	result[len(indexes)] = text[laststart:len(text)]
	return result
}

func writeDirectives(buffer *bytes.Buffer, directives map[string]*directiveData, level int) {
	for _, name := range getSortedKeys(directives) {
		subdirective := directives[name]
		writeDirective(buffer, subdirective, level)
	}
}

func writeDirective(buffer *bytes.Buffer, directive *directiveData, level int) {
	buffer.WriteString(strings.Repeat(" ", level*2))
	if directive.name != "" {
		buffer.WriteString(directive.name)
	}
	if directive.name != "" && len(directive.args) > 0 {
		buffer.WriteString(" ")
	}
	if len(directive.args) > 0 {
		for index, arg := range directive.args {
			if index > 0 {
				buffer.WriteString(" ")
			}
			buffer.WriteString(arg)
		}
	}
	if len(directive.children) > 0 {
		buffer.WriteString(" {\n")
		writeDirectives(buffer, directive.children, level+1)
		buffer.WriteString(strings.Repeat(" ", level*2) + "}")
	}
	buffer.WriteString("\n")
}

func removeSuffix(name string) string {
	return suffixRegex.ReplaceAllString(name, "")
}

func getSortedKeys(m map[string]*directiveData) []string {
	var keys = getKeys(m)
	sort.Strings(keys)
	return keys
}

func getKeys(m map[string]*directiveData) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

type directiveData struct {
	name     string
	args     []string
	children map[string]*directiveData
}

func (directive *directiveData) addArgs(args ...string) {
	directive.args = append(directive.args, args...)
}

func mergeDirectives(directiveA *directiveData, directiveB *directiveData) *directiveData {
	if directiveA == nil {
		return directiveB
	}
	if directiveB == nil {
		return directiveA
	}

	for keyB, subDirectiveB := range directiveB.children {
		if subDirectiveA, exists := directiveA.children[keyB]; exists {
			if subDirectiveA.name == "proxy" &&
				subDirectiveB.name == "proxy" &&
				len(subDirectiveA.args) > 0 &&
				len(subDirectiveB.args) > 0 &&
				subDirectiveA.args[0] == subDirectiveB.args[0] {
				subDirectiveA.addArgs(subDirectiveB.args[1:]...)
				continue
			} else if directivesAreSimilar(subDirectiveA, subDirectiveB) {
				continue
			}

			keyB = removeSuffix(keyB) + "_" + createUniqueSuffix()
		}

		directiveA.children[keyB] = subDirectiveB
	}

	return directiveA
}

func directivesAreSimilar(directiveA *directiveData, directiveB *directiveData) bool {
	if len(directiveA.args) != len(directiveB.args) {
		return false
	}

	for i := 0; i < len(directiveA.args); i++ {
		if directiveA.args[i] != directiveB.args[i] {
			return false
		}
	}

	return true
}

func createUniqueSuffix() string {
	val, err := crand.Int(crand.Reader, big.NewInt(int64(math.MaxInt64)))
	if err != nil {
		return string(rand.Uint64())
	}
	return string(val.Uint64())
}
