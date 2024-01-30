package manager

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/MythicMeta/Mythic_CLI/cmd/config"
	"github.com/MythicMeta/Mythic_CLI/cmd/utils"
	"github.com/creack/pty"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/spf13/viper"
	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v3"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
)

type DockerComposeManager struct {
	InstalledServicesPath   string
	InstalledServicesFolder string
}

// Interface Necessary commands

func (d *DockerComposeManager) GetManagerName() string {
	return "docker"
}

// GenerateRequiredConfig ensure that the docker-compose.yml file exists
func (d *DockerComposeManager) GenerateRequiredConfig() {
	groupNameConfig := viper.New()
	groupNameConfig.SetConfigName("docker-compose")
	groupNameConfig.SetConfigType("yaml")
	groupNameConfig.AddConfigPath(utils.GetCwdFromExe())
	if err := groupNameConfig.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			log.Printf("[-] Error while reading in docker-compose file: %s\n", err)
			if _, err := os.Create("docker-compose.yml"); err != nil {
				log.Fatalf("[-] Failed to create docker-compose.yml file: %v\n", err)
			} else {
				if err := groupNameConfig.ReadInConfig(); err != nil {
					log.Printf("[-] Failed to read in new docker-compose.yml file: %v\n", err)
				} else {
					log.Printf("[+] Successfully created new docker-compose.yml file.\n")
				}
				return
			}
		} else {
			log.Fatalf("[-] Error while parsing docker-compose file: %s", err)
		}
	}
}

// IsServiceRunning use Docker API to check running container list for the specified name
func (d *DockerComposeManager) IsServiceRunning(service string) bool {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("[-] Failed to get client connection to Docker: %v", err)
	}
	containers, err := cli.ContainerList(context.Background(), types.ContainerListOptions{
		All: true,
	})
	if err != nil {
		log.Fatalf("[-] Failed to get container list from Docker: %v", err)
	}
	if len(containers) > 0 {
		for _, container := range containers {
			if container.Labels["name"] == strings.ToLower(service) {
				return true
			}
		}
	}
	return false
}

// DoesImageExist use Docker API to check existing images for the specified name
func (d *DockerComposeManager) DoesImageExist(service string) bool {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("Failed to get client in GetLogs: %v", err)
	}
	desiredImage := fmt.Sprintf("%v:latest", strings.ToLower(service))
	images, err := cli.ImageList(context.Background(), types.ImageListOptions{All: true})
	if err != nil {
		log.Fatalf("Failed to get container list: %v", err)
	}
	for _, image := range images {
		for _, name := range image.RepoTags {
			if name == desiredImage {
				return true
			}
		}
	}
	return false
}

// RemoveImages deletes unused images that aren't tied to any running Docker containers
func (d *DockerComposeManager) RemoveImages() error {
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()

	images, err := cli.ImageList(ctx, types.ImageListOptions{})
	if err != nil {
		log.Fatalf("[-] Failed to get list of images: %v\n", err)
	}

	for _, image := range images {
		if utils.StringInSlice("<none>:<none>", image.RepoTags) {
			_, err = cli.ImageRemove(ctx, image.ID, types.ImageRemoveOptions{
				Force:         true,
				PruneChildren: true,
			})
			if err != nil {
				log.Printf("[-] Failed to remove unused image: %v\n", err)
			}
		}
	}
	return nil
}

func (d *DockerComposeManager) RemoveContainers(services []string) error {
	err := d.runDockerCompose(append([]string{"rm", "-s", "-v", "-f"}, services...))
	if err != nil {
		return err
	}
	_, err = d.runDocker(append([]string{"rm", "-f"}, services...))
	if err != nil {
		return err
	} else {
		return nil
	}
}

func (d *DockerComposeManager) SaveImages(services []string, outputPath string) error {
	savedImagePath := filepath.Join(utils.GetCwdFromExe(), outputPath)
	if !utils.DirExists(savedImagePath) {
		err := os.MkdirAll(savedImagePath, 0755)
		if err != nil {
			log.Fatalf("[-] Failed to create output folder: %v\n", err)
		}
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return errors.New(fmt.Sprintf("[-] Failed to connect to Docker: %v\n", err))
	}
	savedContainers := services
	if len(savedContainers) == 0 {
		diskAgents, err := d.GetInstalled3rdPartyServicesOnDisk()
		if err != nil {
			return errors.New(fmt.Sprintf("[-] Failed to get agents on disk: %v\n", err))
		}
		currentMythicServices, err := d.GetCurrentMythicServiceNames()
		if err != nil {
			return errors.New(fmt.Sprintf("[-] Failed to get mythic service list: %v\n", err))
		}
		savedContainers = append([]string{}, diskAgents...)
		savedContainers = append(savedContainers, currentMythicServices...)

	}
	savedImagePath = filepath.Join(utils.GetCwdFromExe(), "saved_images", "mythic_save.tar")
	finalSavedContainers := []string{}
	for i, _ := range savedContainers {
		if d.DoesImageExist(savedContainers[i]) {
			containerName := fmt.Sprintf("%s:latest", savedContainers[i])
			finalSavedContainers = append(finalSavedContainers, containerName)
		} else {
			log.Printf("[-] No image locally for %s\n", savedContainers[i])
		}
	}
	log.Printf("[*] Saving the following images:\n%v\n", finalSavedContainers)
	log.Printf("[*] This will take a while for Docker to compress and generate the layers...\n")
	ioReadCloser, err := cli.ImageSave(context.Background(), finalSavedContainers)
	if err != nil {
		return errors.New(fmt.Sprintf("[-] Failed to get contents of docker image: %v\n", err))
	}
	outFile, err := os.Create(savedImagePath)
	if err != nil {
		return errors.New(fmt.Sprintf("[-] Failed to create output file: %v\n", err))
	}
	defer outFile.Close()
	log.Printf("[*] Saving to %s\nThis will take a while...\n", savedImagePath)
	_, err = io.Copy(outFile, ioReadCloser)
	if err != nil {
		return errors.New(fmt.Sprintf("[-] Failed to write contents to file: %v\n", err))
	}
	return nil
}

func (d *DockerComposeManager) LoadImages(outputPath string) error {
	savedImagePath := filepath.Join(utils.GetCwdFromExe(), outputPath, "mythic_save.tar")
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return errors.New(fmt.Sprintf("[-] Failed to connect to Docker: %v\n", err))
	}
	ioReadCloser, err := os.OpenFile(savedImagePath, os.O_RDONLY, 0x600)
	if err != nil {
		return errors.New(fmt.Sprintf("[-] Failed to read tar file: %v\n", err))
	}
	_, err = cli.ImageLoad(context.Background(), ioReadCloser, false)
	if err != nil {
		return errors.New(fmt.Sprintf("[-] Failed to load image into Docker: %v\n", err))
	}
	log.Printf("[+] loaded docker images!\n")
	return nil

}

// CheckRequiredManagerVersion checks docker and docker-compose versions to make sure they're high enough
func (d *DockerComposeManager) CheckRequiredManagerVersion() bool {
	outputString, err := d.runDocker([]string{"version", "--format", "{{.Server.Version}}"})
	if err != nil {
		log.Printf("[-] Failed to get docker version\n")
		return false
	}
	if !semver.IsValid("v" + outputString) {
		log.Printf("[-] Invalid version string: %s\n", outputString)
		return false
	}
	if semver.Compare("v"+outputString, "v20.10.22") >= 0 {
		return true
	}
	log.Printf("[-] Docker version is too old, %s, for Mythic. Please update\n", outputString)
	return false

}

// GetVolumes returns a dictionary of defined volume information from the docker-compose file.
//
//	This is not looking up existing volume information at runtime.
func (d *DockerComposeManager) GetVolumes() (map[string]interface{}, error) {
	curConfig := d.readInDockerCompose()
	volumes := map[string]interface{}{}
	if curConfig.InConfig("volumes") {
		volumes = curConfig.GetStringMap("volumes")
	}
	return volumes, nil
}

// SetVolumes sets a specific volume configuration into the docker-compose file.
func (d *DockerComposeManager) SetVolumes(volumes map[string]interface{}) {
	curConfig := d.readInDockerCompose()
	allConfigSettings := curConfig.AllSettings()
	allConfigSettings["volumes"] = volumes
	err := d.setDockerComposeDefaultsAndWrite(allConfigSettings)
	if err != nil {
		log.Printf("[-] Failed to update config: %v\n", err)
	}
}

// GetServiceConfiguration checks docker-compose to see if that service is defined or not and returns its config or a generic one
func (d *DockerComposeManager) GetServiceConfiguration(service string) (map[string]interface{}, error) {
	curConfig := d.readInDockerCompose()
	pStruct := map[string]interface{}{}
	if curConfig.InConfig("services." + strings.ToLower(service)) {
		pStruct = curConfig.GetStringMap("services." + strings.ToLower(service))
		delete(pStruct, "network_mode")
		delete(pStruct, "extra_hosts")
		delete(pStruct, "build")
		delete(pStruct, "networks")
		delete(pStruct, "command")
		delete(pStruct, "image")
		delete(pStruct, "healthcheck")
	} else {
		pStruct = map[string]interface{}{
			"logging": map[string]interface{}{
				"driver": "json-file",
				"options": map[string]string{
					"max-file": "1",
					"max-size": "10m",
				},
			},
			"restart": "always",
			"labels": map[string]string{
				"name": service,
			},
			"container_name": service,
			"image":          service,
		}
	}
	return pStruct, nil
}

// SetServiceConfiguration sets a service configuration into docker-compose
func (d *DockerComposeManager) SetServiceConfiguration(service string, pStruct map[string]interface{}) error {
	curConfig := d.readInDockerCompose()
	allConfigValues := curConfig.AllSettings()
	if _, ok := allConfigValues["services"]; !ok {
		allConfigValues["services"] = map[string]interface{}{}
	}
	for key, _ := range allConfigValues {
		if key == "services" {
			allServices := allConfigValues["services"].(map[string]interface{})
			if _, ok := allServices[service]; !ok {
				log.Printf("[+] Added %s to docker-compose\n", strings.ToLower(service))
			}
			allServices[service] = pStruct
			allConfigValues["services"] = allServices
		}
	}
	err := d.setDockerComposeDefaultsAndWrite(allConfigValues)
	if err != nil {
		log.Printf("[-] Failed to update config: %v\n", err)
	}
	return err
}

// GetPathTo3rdPartyServicesOnDisk returns to path on disk to where 3rd party services are installed
func (d *DockerComposeManager) GetPathTo3rdPartyServicesOnDisk() string {
	return d.InstalledServicesFolder
}

// StopServices stops certain containers that are running and optionally deletes the backing images
func (d *DockerComposeManager) StopServices(services []string, deleteImages bool) error {
	dockerComposeContainers, err := d.GetAllInstalled3rdPartyServiceNames()
	if err != nil {
		return err
	}
	currentMythicServices, err := d.GetCurrentMythicServiceNames()
	if err != nil {
		return err
	}
	// in case somebody says "stop" but doesn't list containers, they mean everything
	if len(services) == 0 {
		services = append(dockerComposeContainers, currentMythicServices...)
	}
	/*
		if utils.StringInSlice("mythic_react", services) {
			if mythicEnv.GetBool("mythic_react_debug") {
				// only need to remove the container if we're switching between debug and regular
				if err = d.runDockerCompose(append([]string{"rm", "-s", "-v", "-f"}, "mythic_react")); err != nil {
					fmt.Printf("[-] Failed to remove mythic_react\n")
					return err
				}
			}
		}

	*/
	if deleteImages {
		return d.runDockerCompose(append([]string{"rm", "-s", "-v", "-f"}, services...))
	} else {
		return d.runDockerCompose(append([]string{"stop"}, services...))
	}

}

// RemoveServices removes certain container entries from the docker-compose
func (d *DockerComposeManager) RemoveServices(services []string) error {
	curConfig := d.readInDockerCompose()
	allConfigValues := curConfig.AllSettings()
	for key, _ := range allConfigValues {
		if key == "services" {
			allServices := allConfigValues["services"].(map[string]interface{})
			for _, service := range services {
				if d.IsServiceRunning(service) {
					_ = d.StopServices([]string{strings.ToLower(service)}, true)

				}
				delete(allServices, strings.ToLower(service))
				log.Printf("[+] Removed %s from docker-compose\n", strings.ToLower(service))
			}
		}
	}

	err := d.setDockerComposeDefaultsAndWrite(allConfigValues)
	if err != nil {
		log.Printf("[-] Failed to update config: %v\n", err)
		return err
	} else {
		log.Println("[+] Successfully updated docker-compose.yml")
	}
	return nil
}

// StartServices kicks off docker/docker-compose for the specified services
func (d *DockerComposeManager) StartServices(services []string, rebuildOnStart bool) error {

	if rebuildOnStart {
		err := d.runDockerCompose(append([]string{"up", "--build", "-d"}, services...))
		if err != nil {
			return err
		}
	} else {
		var needToBuild []string
		var alreadyBuilt []string
		for _, val := range services {
			if !d.DoesImageExist(val) {
				needToBuild = append(needToBuild, val)
			} else {
				alreadyBuilt = append(alreadyBuilt, val)
			}
		}
		if len(needToBuild) > 0 {
			if err := d.runDockerCompose(append([]string{"up", "--build", "-d"}, needToBuild...)); err != nil {
				return err
			}
		}
		if len(alreadyBuilt) > 0 {
			if err := d.runDockerCompose(append([]string{"up", "-d"}, alreadyBuilt...)); err != nil {
				return err
			}
		}
	}

	return nil

}

// BuildServices rebuilds services images and creates containers based on those images
func (d *DockerComposeManager) BuildServices(services []string) error {
	if len(services) == 0 {
		return nil
	}

	err := d.runDockerCompose(append([]string{"rm", "-s", "-v", "-f"}, services...))
	if err != nil {
		return err
	}
	err = d.runDockerCompose(append([]string{"up", "--build", "-d"}, services...))
	if err != nil {
		return err
	}
	return nil

}

// GetInstalled3rdPartyServicesOnDisk lists out the name of all 3rd party software installed on disk
func (d *DockerComposeManager) GetInstalled3rdPartyServicesOnDisk() ([]string, error) {
	var agentsOnDisk []string
	if !utils.DirExists(d.InstalledServicesFolder) {
		if err := os.Mkdir(d.InstalledServicesFolder, 0775); err != nil {
			return nil, err
		}
	}
	if files, err := os.ReadDir(d.InstalledServicesFolder); err != nil {
		log.Printf("[-] Failed to list contents of %s folder\n", d.InstalledServicesFolder)
		return nil, err
	} else {
		for _, f := range files {
			if f.IsDir() {
				agentsOnDisk = append(agentsOnDisk, f.Name())
			}
		}
	}
	return agentsOnDisk, nil
}

func (d *DockerComposeManager) GetHealthCheck(services []string) {
	for _, container := range services {
		outputString, err := d.runDocker([]string{"inspect", "--format", "{{json .State.Health }}", container})
		if err != nil {
			log.Printf("failed to check status: %s", err.Error())
		} else {
			log.Printf("%s:\n%s\n\n", container, outputString)
		}
	}
}

func (d *DockerComposeManager) BuildUI() error {
	_, err := d.runDocker([]string{"exec", "mythic_react", "/bin/sh", "-c", "npm run react-build"})
	if err != nil {
		log.Printf("[-] Failed to build new UI from MythicReactUI: %v\n", err)
	}
	return err
}

func (d *DockerComposeManager) GetLogs(service string, logCount int, follow bool) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("Failed to get client in GetLogs: %v", err)
	}
	containers, err := cli.ContainerList(context.Background(), types.ContainerListOptions{})
	if err != nil {
		log.Fatalf("Failed to get container list: %v", err)
	}
	if len(containers) > 0 {
		found := false
		for _, container := range containers {
			if container.Labels["name"] == service {
				found = true
				reader, err := cli.ContainerLogs(context.Background(), container.ID, types.ContainerLogsOptions{
					ShowStdout: true,
					ShowStderr: true,
					Follow:     follow,
					Tail:       fmt.Sprintf("%d", logCount),
				})
				if err != nil {
					log.Fatalf("Failed to get container GetLogs: %v", err)
				}
				// awesome post about the leading 8 payload/header bytes: https://medium.com/@dhanushgopinath/reading-docker-container-logs-with-golang-docker-engine-api-702233fac044
				p := make([]byte, 8)
				_, err = reader.Read(p)
				for err == nil {
					content := make([]byte, binary.BigEndian.Uint32(p[4:]))
					reader.Read(content)
					fmt.Printf("%s", content)
					_, err = reader.Read(p)
				}
				reader.Close()
			}
		}
		if !found {
			log.Println("[-] Failed to find that container")
		}
	} else {
		log.Println("[-] No containers running")
	}
}

func (d *DockerComposeManager) TestPorts(services []string) {
	// go through the different services in mythicEnv and check to make sure their ports aren't already used by trying to open them
	//MYTHIC_SERVER_HOST:MYTHIC_SERVER_PORT
	//POSTGRES_HOST:POSTGRES_PORT
	//HASURA_HOST:HASURA_PORT
	//RABBITMQ_HOST:RABBITMQ_PORT
	//DOCUMENTATION_HOST:DOCUMENTATION_PORT
	//NGINX_HOST:NGINX_PORT
	portChecks := map[string][]string{
		"MYTHIC_SERVER_HOST": {
			"MYTHIC_SERVER_PORT",
			"mythic_server",
		},
		"POSTGRES_HOST": {
			"POSTGRES_PORT",
			"mythic_postgres",
		},
		"HASURA_HOST": {
			"HASURA_PORT",
			"mythic_graphql",
		},
		"RABBITMQ_HOST": {
			"RABBITMQ_PORT",
			"mythic_rabbitmq",
		},
		"DOCUMENTATION_HOST": {
			"DOCUMENTATION_PORT",
			"mythic_documentation",
		},
		"NGINX_HOST": {
			"NGINX_PORT",
			"mythic_nginx",
		},
		"MYTHIC_REACT_HOST": {
			"MYTHIC_REACT_PORT",
			"mythic_react",
		},
		"JUPYTER_HOST": {
			"JUPYTER_PORT",
			"mythic_jupyter",
		},
	}
	var addServices []string
	var removeServices []string
	mythicEnv := config.GetMythicEnv()
	for key, val := range portChecks {
		// only check ports for services we're about to start
		if utils.StringInSlice(val[1], services) {
			if mythicEnv.GetString(key) == val[1] || mythicEnv.GetString(key) == "127.0.0.1" {
				addServices = append(addServices, val[1])
				p, err := net.Listen("tcp", ":"+strconv.Itoa(mythicEnv.GetInt(val[0])))
				if err != nil {
					log.Fatalf("[-] Port %d, from variable %s, appears to already be in use: %v\n", mythicEnv.GetInt(val[0]), key, err)
				}
				err = p.Close()
				if err != nil {
					log.Printf("[-] Failed to close connection: %v\n", err)
				}
			} else {
				removeServices = append(removeServices, val[1])
			}
		}

	}
}

func (d *DockerComposeManager) PrintConnectionInfo() {
	w := new(tabwriter.Writer)
	mythicEnv := config.GetMythicEnv()
	w.Init(os.Stdout, 0, 8, 2, '\t', 0)
	fmt.Fprintln(w, "MYTHIC SERVICE\tWEB ADDRESS\tBOUND LOCALLY")
	if mythicEnv.GetString("NGINX_HOST") == "mythic_nginx" {
		if mythicEnv.GetBool("NGINX_USE_SSL") {
			fmt.Fprintln(w, "Nginx (Mythic Web UI)\thttps://127.0.0.1:"+strconv.Itoa(mythicEnv.GetInt("NGINX_PORT"))+"\t", mythicEnv.GetBool("nginx_bind_localhost_only"))
		} else {
			fmt.Fprintln(w, "Nginx (Mythic Web UI)\thttp://127.0.0.1:"+strconv.Itoa(mythicEnv.GetInt("NGINX_PORT"))+"\t", mythicEnv.GetBool("nginx_bind_localhost_only"))
		}
	} else {
		if mythicEnv.GetBool("NGINX_USE_SSL") {
			fmt.Fprintln(w, "Nginx (Mythic Web UI)\thttps://"+mythicEnv.GetString("NGINX_HOST")+":"+strconv.Itoa(mythicEnv.GetInt("NGINX_PORT"))+"\t", mythicEnv.GetBool("nginx_bind_localhost_only"))
		} else {
			fmt.Fprintln(w, "Nginx (Mythic Web UI)\thttp://"+mythicEnv.GetString("NGINX_HOST")+":"+strconv.Itoa(mythicEnv.GetInt("NGINX_PORT"))+"\t", mythicEnv.GetBool("nginx_bind_localhost_only"))
		}
	}
	if mythicEnv.GetString("MYTHIC_SERVER_HOST") == "mythic_server" {
		fmt.Fprintln(w, "Mythic Backend Server\thttp://127.0.0.1:"+strconv.Itoa(mythicEnv.GetInt("MYTHIC_SERVER_PORT"))+"\t", mythicEnv.GetBool("mythic_server_bind_localhost_only"))
	} else {
		fmt.Fprintln(w, "Mythic Backend Server\thttp://"+mythicEnv.GetString("MYTHIC_SERVER_HOST")+":"+strconv.Itoa(mythicEnv.GetInt("MYTHIC_SERVER_PORT"))+"\t", mythicEnv.GetBool("mythic_server_bind_localhost_only"))
	}
	if mythicEnv.GetString("HASURA_HOST") == "mythic_graphql" {
		fmt.Fprintln(w, "Hasura GraphQL Console\thttp://127.0.0.1:"+strconv.Itoa(mythicEnv.GetInt("HASURA_PORT"))+"\t", mythicEnv.GetBool("hasura_bind_localhost_only"))
	} else {
		fmt.Fprintln(w, "Hasura GraphQL Console\thttp://"+mythicEnv.GetString("HASURA_HOST")+":"+strconv.Itoa(mythicEnv.GetInt("HASURA_PORT"))+"\t", mythicEnv.GetBool("hasura_bind_localhost_only"))
	}
	if mythicEnv.GetString("JUPYTER_HOST") == "mythic_jupyter" {
		fmt.Fprintln(w, "Jupyter Console\thttp://127.0.0.1:"+strconv.Itoa(mythicEnv.GetInt("JUPYTER_PORT"))+"\t", mythicEnv.GetBool("jupyter_bind_localhost_only"))
	} else {
		fmt.Fprintln(w, "Jupyter Console\thttp://"+mythicEnv.GetString("JUPYTER_HOST")+":"+strconv.Itoa(mythicEnv.GetInt("JUPYTER_PORT"))+"\t", mythicEnv.GetBool("jupyter_bind_localhost_only"))
	}
	if mythicEnv.GetString("DOCUMENTATION_HOST") == "mythic_documentation" {
		fmt.Fprintln(w, "Internal Documentation\thttp://127.0.0.1:"+strconv.Itoa(mythicEnv.GetInt("DOCUMENTATION_PORT"))+"\t", mythicEnv.GetBool("documentation_bind_localhost_only"))
	} else {
		fmt.Fprintln(w, "Internal Documentation\thttp://"+mythicEnv.GetString("DOCUMENTATION_HOST")+":"+strconv.Itoa(mythicEnv.GetInt("DOCUMENTATION_PORT"))+"\t", mythicEnv.GetBool("documentation_bind_localhost_only"))
	}
	fmt.Fprintln(w, "\t\t\t\t")
	fmt.Fprintln(w, "ADDITIONAL SERVICES\tADDRESS\tBOUND LOCALLY")
	if mythicEnv.GetString("POSTGRES_HOST") == "mythic_postgres" {
		fmt.Fprintln(w, "Postgres Database\tpostgresql://mythic_user:password@127.0.0.1:"+strconv.Itoa(mythicEnv.GetInt("POSTGRES_PORT"))+"/mythic_db\t", mythicEnv.GetBool("postgres_bind_localhost_only"))
	} else {
		fmt.Fprintln(w, "Postgres Database\tpostgresql://mythic_user:password@"+mythicEnv.GetString("POSTGRES_HOST")+":"+strconv.Itoa(mythicEnv.GetInt("POSTGRES_PORT"))+"/mythic_db\t", mythicEnv.GetBool("postgres_bind_localhost_only"))
	}
	if mythicEnv.GetString("MYTHIC_REACT_HOST") == "mythic_react" {
		fmt.Fprintln(w, "React Server\thttp://127.0.0.1:"+strconv.Itoa(mythicEnv.GetInt("MYTHIC_REACT_PORT"))+"/new\t", mythicEnv.GetBool("mythic_react_bind_localhost_only"))
	} else {
		fmt.Fprintln(w, "React Server\thttp://"+mythicEnv.GetString("MYTHIC_REACT_HOST")+":"+strconv.Itoa(mythicEnv.GetInt("MYTHIC_REACT_PORT"))+"/new\t", mythicEnv.GetBool("mythic_react_bind_localhost_only"))
	}
	if mythicEnv.GetString("RABBITMQ_HOST") == "mythic_rabbitmq" {
		fmt.Fprintln(w, "RabbitMQ\tamqp://"+mythicEnv.GetString("RABBITMQ_USER")+":password@127.0.0.1:"+strconv.Itoa(mythicEnv.GetInt("RABBITMQ_PORT"))+"\t", mythicEnv.GetBool("rabbitmq_bind_localhost_only"))
	} else {
		fmt.Fprintln(w, "RabbitMQ\tamqp://"+mythicEnv.GetString("RABBITMQ_USER")+":password@"+mythicEnv.GetString("RABBITMQ_HOST")+":"+strconv.Itoa(mythicEnv.GetInt("RABBITMQ_PORT"))+"\t", mythicEnv.GetBool("rabbitmq_bind_localhost_only"))
	}
	fmt.Fprintln(w, "\t\t\t\t")
	w.Flush()
}

func (d *DockerComposeManager) Status(verbose bool) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("[-] Failed to get client in Status check: %v", err)
	}
	containers, err := cli.ContainerList(context.Background(), types.ContainerListOptions{
		All: true,
	})
	if err != nil {
		log.Fatalf("[-] Failed to get container list: %v\n", err)
	}
	w := new(tabwriter.Writer)
	w.Init(os.Stdout, 0, 8, 2, '\t', 0)
	var mythicLocalServices []string
	var installedServices []string
	sort.Slice(containers[:], func(i, j int) bool {
		return containers[i].Labels["name"] < containers[j].Labels["name"]
	})
	elementsOnDisk, err := d.GetInstalled3rdPartyServicesOnDisk()
	if err != nil {
		log.Fatalf("[-] Failed to get list of installed services on disk: %v\n", err)
	}
	elementsInCompose, err := d.GetAllInstalled3rdPartyServiceNames()
	if err != nil {
		log.Fatalf("[-] Failed to get list of installed services in docker-compose: %v\n", err)
	}
	for _, container := range containers {
		if container.Labels["name"] == "" {
			continue
		}
		var portRanges []uint16
		var portRangeMaps []string
		portString := ""
		info := fmt.Sprintf("%s\t%s\t%s\t", container.Labels["name"], container.State, container.Status)
		if len(container.Ports) > 0 {
			sort.Slice(container.Ports[:], func(i, j int) bool {
				return container.Ports[i].PublicPort < container.Ports[j].PublicPort
			})
			for _, port := range container.Ports {
				if port.PublicPort > 0 {
					if port.PrivatePort == port.PublicPort && port.IP == "0.0.0.0" {
						portRanges = append(portRanges, port.PrivatePort)
					} else {
						portRangeMaps = append(portRangeMaps, fmt.Sprintf("%d/%s -> %s:%d", port.PrivatePort, port.Type, port.IP, port.PublicPort))
					}

				}
			}
			if len(portRanges) > 0 {
				sort.Slice(portRanges, func(i, j int) bool { return portRanges[i] < portRanges[j] })
			}
			portString = strings.Join(portRangeMaps[:], ", ")
			var stringPortRanges []string
			for _, val := range portRanges {
				stringPortRanges = append(stringPortRanges, fmt.Sprintf("%d", val))
			}
			if len(stringPortRanges) > 0 && len(portString) > 0 {
				portString = portString + ", "
			}
			portString = portString + strings.Join(stringPortRanges[:], ", ")
		}
		foundMountInfo := false
		for _, mnt := range container.Mounts {
			if strings.HasPrefix(mnt.Name, container.Labels["name"]+"_volume") {
				if foundMountInfo {
					info += ", " + mnt.Name
				} else {
					info += mnt.Name
				}
				foundMountInfo = true
			}
		}
		if !foundMountInfo {
			info += "local"
		}
		info += "\t"
		if utils.StringInSlice(container.Labels["name"], config.MythicPossibleServices) {
			info = info + portString
			mythicLocalServices = append(mythicLocalServices, info)
		} else {
			if utils.StringInSlice(container.Labels["name"], elementsOnDisk) ||
				utils.StringInSlice(container.Labels["name"], elementsInCompose) {
				installedServices = append(installedServices, info)
				elementsOnDisk = utils.RemoveStringFromSliceNoOrder(elementsOnDisk, container.Labels["name"])
				elementsInCompose = utils.RemoveStringFromSliceNoOrder(elementsInCompose, container.Labels["name"])
			}
		}
	}
	fmt.Fprintln(w, "Mythic Main Services")
	fmt.Fprintln(w, "CONTAINER NAME\tSTATE\tSTATUS\tMOUNT\tPORTS")
	for _, line := range mythicLocalServices {
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w, "\t\t\t\t\t")
	w.Flush()
	fmt.Fprintln(w, "Installed Services")
	fmt.Fprintln(w, "CONTAINER NAME\tSTATE\tSTATUS\tMOUNT")
	for _, line := range installedServices {
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w, "\t\t\t\t")
	// remove all elementsInCompose from elementsOnDisk
	for _, container := range elementsInCompose {
		elementsOnDisk = utils.RemoveStringFromSliceNoOrder(elementsOnDisk, container)
	}
	if len(elementsInCompose) > 0 && verbose {
		fmt.Fprintln(w, "Docker Compose services not running, start with: ./mythic-cli start [name]")
		fmt.Fprintln(w, "NAME\t")
		sort.Strings(elementsInCompose)
		for _, container := range elementsInCompose {
			fmt.Fprintln(w, fmt.Sprintf("%s\t", container))
		}
		fmt.Fprintln(w, "\t")
	}
	if len(elementsOnDisk) > 0 && verbose {
		fmt.Fprintln(w, "Extra Services, add to docker compose with: ./mythic-cli add [name]")
		fmt.Fprintln(w, "NAME\t")
		sort.Strings(elementsOnDisk)
		for _, container := range elementsOnDisk {
			fmt.Fprintln(w, fmt.Sprintf("%s\t", container))
		}
		fmt.Fprintln(w, "\t\t")
	}
	w.Flush()
}

func (d *DockerComposeManager) PrintAllServices() {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("[-] Failed to get client in List Services: %v", err)
	}
	containers, err := cli.ContainerList(context.Background(), types.ContainerListOptions{
		All: true,
	})
	if err != nil {
		log.Fatalf("[-] Failed to get container list: %v\n", err)
	}
	w := new(tabwriter.Writer)
	w.Init(os.Stdout, 0, 8, 2, '\t', 0)
	var installedServices []string
	sort.Slice(containers[:], func(i, j int) bool {
		return containers[i].Labels["name"] < containers[j].Labels["name"]
	})
	elementsOnDisk, err := d.GetInstalled3rdPartyServicesOnDisk()
	if err != nil {
		log.Fatalf("[-] Failed to get list of installed services on disk: %v\n", err)
	}
	elementsInCompose, err := d.GetAllInstalled3rdPartyServiceNames()
	if err != nil {
		log.Fatalf("[-] Failed to get list of installed services in docker-compose: %v\n", err)
	}
	for _, container := range containers {
		if container.Labels["name"] == "" {
			continue
		}
		for _, mnt := range container.Mounts {
			if strings.Contains(mnt.Source, d.InstalledServicesPath) {
				info := fmt.Sprintf("%s\t%s\t%v\t%v", container.Labels["name"], container.Status, true, utils.StringInSlice(container.Labels["name"], elementsInCompose))
				installedServices = append(installedServices, info)
				elementsOnDisk = utils.RemoveStringFromSliceNoOrder(elementsOnDisk, container.Labels["name"])
				elementsInCompose = utils.RemoveStringFromSliceNoOrder(elementsInCompose, container.Labels["name"])
			}
		}
	}
	for _, container := range elementsInCompose {
		elementsOnDisk = utils.RemoveStringFromSliceNoOrder(elementsOnDisk, container)
	}
	fmt.Fprintln(w, "Name\tContainerStatus\tImageBuilt\tDockerComposeEntry")
	for _, line := range installedServices {
		fmt.Fprintln(w, line)
	}
	if len(elementsInCompose) > 0 {
		sort.Strings(elementsInCompose)
		for _, container := range elementsInCompose {
			fmt.Fprintln(w, fmt.Sprintf("%s\t%s\t%v\t%v", container, "N/A", d.DoesImageExist(container), true))
		}
	}
	if len(elementsOnDisk) > 0 {
		sort.Strings(elementsOnDisk)
		for _, container := range elementsOnDisk {
			fmt.Fprintln(w, fmt.Sprintf("%s\t%s\t%v\t%v", container, "N/A", d.DoesImageExist(container), false))
		}
	}
	w.Flush()
}

func (d *DockerComposeManager) ResetDatabase(useVolume bool) {
	if !useVolume {
		workingPath := utils.GetCwdFromExe()
		err := os.RemoveAll(filepath.Join(workingPath, "postgres-docker", "database"))
		if err != nil {
			log.Fatalf("[-] Failed to remove database files\n%v\n", err)
		} else {
			log.Printf("[+] Successfully reset datbase files\n")
		}
	} else {
		_ = d.RemoveContainers([]string{"mythic_postgres"})
		err := d.RemoveVolume("mythic_postgres_volume")
		if err != nil {
			log.Printf("[-] Failed to remove database:\n%v\n", err)
		}
	}
}
func (d *DockerComposeManager) BackupDatabase(backupPath string, useVolume bool) {
	/*
		if !useVolume {
			workingPath := utils.GetCwdFromExe()
			err := utils.CopyDir(filepath.Join(workingPath, "postgres-docker", "database"), backupPath)
			if err != nil {
				log.Fatalf("[-] Failed to copy database files\n%v\n", err)
			} else {
				log.Printf("[+] Successfully copied datbase files\n")
			}
		} else {
			d.CopyFromVolume("mythic_postgres_volume", "/var/lib/postgresql/data", backupPath)
			log.Printf("[+] Successfully copied database files")
		}

	*/
}
func (d *DockerComposeManager) RestoreDatabase(backupPath string, useVolume bool) {
	/*
		if !useVolume {
			workingPath := utils.GetCwdFromExe()
			err := utils.CopyDir(backupPath, filepath.Join(workingPath, "postgres-docker", "database"))
			if err != nil {
				log.Fatalf("[-] Failed to copy database files\n%v\n", err)
			} else {
				log.Printf("[+] Successfully copied datbase files\n")
			}
		} else {
			d.CopyIntoVolume("mythic_postgres_volume", "/var/lib/postgresql/data", backupPath)
			log.Printf("[+] Successfully copied database files")
		}

	*/
}
func (d *DockerComposeManager) PrintVolumeInformation() {
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		panic(err)
	}
	defer cli.Close()
	w := new(tabwriter.Writer)
	w.Init(os.Stdout, 0, 8, 2, '\t', 0)
	fmt.Fprintln(w, "VOLUME\tSIZE\tCONTAINER (Ref Count)\tCONTAINER STATUS\tLOCATION")
	du, err := cli.DiskUsage(ctx, types.DiskUsageOptions{})
	if err != nil {
		log.Fatalf("[-] Failed to get disk sizes: %v\n", err)
	}
	containers, err := cli.ContainerList(ctx, types.ContainerListOptions{Size: true})
	if err != nil {
		log.Fatalf("[-] Failed to get container list: %v\n", err)
	}
	if du.Volumes == nil {
		log.Printf("[-] No volumes known\n")
		return
	}
	var entries []string
	volumeList, err := d.GetVolumes()
	if err != nil {
		log.Fatalf("[-] Failed to get volumes: %v", err)
	}
	for _, currentVolume := range du.Volumes {
		name := currentVolume.Name
		size := "unknown"
		if currentVolume.UsageData != nil {
			size = utils.ByteCountSI(currentVolume.UsageData.Size)
		}
		if _, ok := volumeList[currentVolume.Name]; !ok {
			continue
		}
		containerPieces := strings.Split(currentVolume.Name, "_volume")
		containerName := containerPieces[0]
		container := "unused (0)"
		containerStatus := "offline"
		for _, c := range containers {
			if containerName == c.Labels["name"] {
				containerStatus = c.Status
			}
			for _, m := range c.Mounts {
				if m.Name == currentVolume.Name {
					container = containerName + " (" + strconv.Itoa(int(currentVolume.UsageData.RefCount)) + ")"
				}
			}
		}
		entries = append(entries, fmt.Sprintf("%s\t%s\t%s\t%s\t%s",
			name,
			size,
			container,
			containerStatus,
			currentVolume.Mountpoint,
		))
	}
	sort.Strings(entries)
	for _, line := range entries {
		fmt.Fprintln(w, line)
	}

	defer w.Flush()
	return
}
func (d *DockerComposeManager) RemoveVolume(volumeName string) error {
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		panic(err)
	}
	defer cli.Close()
	volumes, err := cli.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return err
	}
	for _, currentVolume := range volumes.Volumes {
		if currentVolume.Name == volumeName {
			containers, err := cli.ContainerList(ctx, types.ContainerListOptions{Size: true})
			if err != nil {
				log.Fatalf("[-] Failed to get container list: %v\n", err)
			}
			for _, c := range containers {
				for _, m := range c.Mounts {
					if m.Name == volumeName {
						containerName := c.Labels["name"]
						err = cli.ContainerRemove(ctx, c.ID, types.ContainerRemoveOptions{Force: true})
						if err != nil {
							log.Printf(fmt.Sprintf("[!] Failed to remove container that's using the volume: %v\n", err))
						} else {
							log.Printf("[+] Removed container %s, which was using that volume", containerName)
						}
					}
				}
			}
			err = cli.VolumeRemove(ctx, currentVolume.Name, true)
			return err
		}
	}
	log.Printf("[*] Volume not found")
	return nil
}
func (d *DockerComposeManager) CopyIntoVolume(sourceFile io.Reader, destinationFileName string, destinationVolume string) {
	err := d.ensureVolume(destinationVolume)
	if err != nil {
		log.Fatalf("[-] Failed to ensure volume exists: %v\n", err)
	}
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("[-] Failed to connect to docker api: %v\n", err)
	}
	defer cli.Close()
	containers, err := cli.ContainerList(ctx, types.ContainerListOptions{Size: true})
	if err != nil {
		log.Fatalf("[-] Failed to get container list: %v\n", err)
	}
	for _, container := range containers {
		for _, mnt := range container.Mounts {
			if mnt.Name == destinationVolume {
				err = cli.CopyToContainer(ctx, container.ID, mnt.Destination+"/"+destinationFileName, sourceFile, types.CopyToContainerOptions{
					CopyUIDGID: true,
				})
				if err != nil {
					log.Fatalf("[-] Failed to write file: %v\n", err)
				} else {
					log.Printf("[+] Successfully wrote file\n")
				}
				return
			}
		}
	}
	log.Fatalf("[-] Failed to find that volume name in use by any containers")
}
func (d *DockerComposeManager) CopyFromVolume(sourceVolumeName string, sourceFileName string, destinationName string) {
	err := d.ensureVolume(sourceVolumeName)
	if err != nil {
		log.Fatalf("[-] Failed to ensure volume exists: %v\n", err)
	}
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("[-] Failed to connect to docker api: %v\n", err)
	}
	defer cli.Close()
	containers, err := cli.ContainerList(ctx, types.ContainerListOptions{Size: true})
	if err != nil {
		log.Fatalf("[-] Failed to get container list: %v\n", err)
	}
	for _, container := range containers {
		for _, mnt := range container.Mounts {
			if mnt.Name == sourceVolumeName {
				reader, _, err := cli.CopyFromContainer(ctx, container.ID, mnt.Destination+"/"+sourceFileName)
				if err != nil {
					log.Printf("[-] Failed to read file: %v\n", err)
					return
				}
				destination, err := os.Create(destinationName)
				if err != nil {
					log.Printf("[-] Failed to open destination filename: %v\n", err)
					return
				}
				_, err = io.Copy(destination, reader)
				destination.Close()
				if err != nil {
					log.Printf("[-] Failed to get file from volume: %v\n", err)
					return
				}
				log.Printf("[+] Successfully wrote file\n")
				return
			}
		}
	}
	log.Fatalf("[-] Failed to find that volume name in use by any containers")
}

// Internal Support Commands
func (d *DockerComposeManager) getMythicEnvList() []string {
	env := config.GetMythicEnv().AllSettings()
	var envList []string
	for key := range env {
		val := config.GetMythicEnv().GetString(key)
		if val != "" {
			// prevent trying to append arrays or dictionaries to our environment list
			envList = append(envList, strings.ToUpper(key)+"="+val)
		}
	}
	envList = append(envList, os.Environ()...)
	return envList
}
func (d *DockerComposeManager) getCwdFromExe() string {
	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("[-] Failed to get path to current executable\n")
	}
	return filepath.Dir(exe)
}
func (d *DockerComposeManager) runDocker(args []string) (string, error) {
	lookPath, err := exec.LookPath("docker")
	if err != nil {
		log.Fatalf("[-] docker is not installed or available in the current PATH\n")
	}
	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("[-] Failed to get lookPath to current executable\n")
	}
	exePath := filepath.Dir(exe)
	command := exec.Command(lookPath, args...)
	command.Dir = exePath
	command.Env = d.getMythicEnvList()
	stdout, err := command.StdoutPipe()
	if err != nil {
		log.Fatalf("[-] Failed to get stdout pipe for running docker-compose\n")
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		log.Fatalf("[-] Failed to get stderr pipe for running docker-compose\n")
	}
	stdoutScanner := bufio.NewScanner(stdout)
	stderrScanner := bufio.NewScanner(stderr)
	outputString := ""
	wg := sync.WaitGroup{}
	wg.Add(2)
	go func() {
		for stdoutScanner.Scan() {
			outputString += stdoutScanner.Text()
		}
		wg.Done()
	}()
	go func() {
		for stderrScanner.Scan() {
			fmt.Printf("%s\n", stderrScanner.Text())
		}
		wg.Done()
	}()
	err = command.Start()
	if err != nil {
		log.Fatalf("[-] Error trying to start docker: %v\n", err)
	}
	wg.Wait()
	err = command.Wait()
	if err != nil {
		log.Printf("[-] Error from docker: %v\n", err)
		log.Printf("[*] Docker command: %v\n", args)
		return "", err
	}
	return outputString, nil
}
func (d *DockerComposeManager) runDockerCompose(args []string) error {
	lookPath, err := exec.LookPath("docker-compose")
	if err != nil {
		lookPath, err = exec.LookPath("docker")
		if err != nil {
			log.Fatalf("[-] docker-compose and docker are not installed or available in the current PATH\n")
		} else {
			// adjust the current args for docker compose subcommand
			args = append([]string{"compose"}, args...)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("[-] Failed to get lookPath to current executable\n")
	}
	exePath := filepath.Dir(exe)
	command := exec.Command(lookPath, args...)
	command.Dir = exePath
	command.Env = d.getMythicEnvList()
	f, err := pty.Start(command)
	if err != nil {
		stdout, err := command.StdoutPipe()
		if err != nil {
			log.Fatalf("[-] Failed to get stdout pipe for running docker-compose\n")
		}
		stderr, err := command.StderrPipe()
		if err != nil {
			log.Fatalf("[-] Failed to get stderr pipe for running docker-compose\n")
		}

		stdoutScanner := bufio.NewScanner(stdout)
		stderrScanner := bufio.NewScanner(stderr)
		go func() {
			for stdoutScanner.Scan() {
				fmt.Printf("%s\n", stdoutScanner.Text())
			}
		}()
		go func() {
			for stderrScanner.Scan() {
				fmt.Printf("%s\n", stderrScanner.Text())
			}
		}()
		err = command.Start()
		if err != nil {
			log.Fatalf("[-] Error trying to start docker-compose: %v\n", err)
		}
		err = command.Wait()
		if err != nil {
			fmt.Printf("[-] Error from docker-compose: %v\n", err)
			fmt.Printf("[*] Docker compose command: %v\n", args)
			return err
		}
	} else {
		io.Copy(os.Stdout, f)
	}

	return nil
}
func (d *DockerComposeManager) setDockerComposeDefaultsAndWrite(curConfig map[string]interface{}) error {
	file := filepath.Join(utils.GetCwdFromExe(), "docker-compose.yml")
	curConfig["version"] = "2.4"
	delete(curConfig, "networks")
	content, err := yaml.Marshal(curConfig)
	if err != nil {
		return err
	}
	return os.WriteFile(file, content, 0644)
}
func (d *DockerComposeManager) readInDockerCompose() *viper.Viper {
	var curConfig = viper.New()
	curConfig.SetConfigName("docker-compose")
	curConfig.SetConfigType("yaml")
	curConfig.AddConfigPath(d.getCwdFromExe())
	if err := curConfig.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			log.Fatalf("[-] Error while reading in docker-compose file: %s\n", err)
		} else {
			log.Fatalf("[-] Error while parsing docker-compose file: %s\n", err)
		}
	}
	return curConfig
}
func (d *DockerComposeManager) ensureVolume(volumeName string) error {
	containerNamePieces := strings.Split(volumeName, "_")
	containerName := strings.Join(containerNamePieces[0:2], "_")
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()
	volumes, err := cli.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return err
	}
	foundVolume := false
	for _, currentVolume := range volumes.Volumes {
		if currentVolume.Name == volumeName {
			foundVolume = true
		}
	}
	if !foundVolume {
		_, err = cli.VolumeCreate(ctx, volume.CreateOptions{Name: volumeName})
		if err != nil {
			return err
		}
	}
	// now that we know the volume exists, make sure it's attached to a running container or we can't manipulate files
	containers, err := cli.ContainerList(ctx, types.ContainerListOptions{Size: true})
	if err != nil {
		return err
	}
	for _, container := range containers {
		if container.Image == containerName {
			for _, mnt := range container.Mounts {
				if mnt.Name == volumeName {
					// container is running and has this mount associated with it
					return nil
				}
			}
			return errors.New(fmt.Sprintf("container, %s, isn't using volume, %s", containerName, volumeName))
		}
	}
	return errors.New(fmt.Sprintf("failed to find container, %s, for volume, %s", containerName, volumeName))
}

func (d *DockerComposeManager) GetAllInstalled3rdPartyServiceNames() ([]string, error) {
	// get all services that exist within the loaded config
	groupNameConfig := viper.New()
	groupNameConfig.SetConfigName("docker-compose")
	groupNameConfig.SetConfigType("yaml")
	groupNameConfig.AddConfigPath(utils.GetCwdFromExe())
	if err := groupNameConfig.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			log.Printf("[-] Error while reading in docker-compose file: %s\n", err)
			return []string{}, err
		} else {
			log.Printf("[-] Error while parsing docker-compose file: %s\n", err)
			return []string{}, err
		}
	}
	servicesSub := groupNameConfig.Sub("services")
	containerList := []string{}
	if servicesSub != nil {
		services := servicesSub.AllSettings()
		for service := range services {
			if !utils.StringInSlice(service, config.MythicPossibleServices) {
				containerList = append(containerList, service)
			}
		}
	}

	return containerList, nil
}

// GetCurrentMythicServiceNames from reading in the docker-compose file, not necessarily what should be there or what's running
func (d *DockerComposeManager) GetCurrentMythicServiceNames() ([]string, error) {
	groupNameConfig := viper.New()
	groupNameConfig.SetConfigName("docker-compose")
	groupNameConfig.SetConfigType("yaml")
	groupNameConfig.AddConfigPath(utils.GetCwdFromExe())
	if err := groupNameConfig.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			log.Printf("[-] Error while reading in docker-compose file: %s\n", err)
			return []string{}, err
		} else {
			log.Printf("[-] Error while parsing docker-compose file: %s\n", err)
			return []string{}, err
		}
	}
	servicesSub := groupNameConfig.Sub("services")

	containerList := []string{}
	if servicesSub != nil {
		services := servicesSub.AllSettings()
		for service := range services {
			if utils.StringInSlice(service, config.MythicPossibleServices) {
				containerList = append(containerList, service)
			}
		}
	}

	return containerList, nil
}