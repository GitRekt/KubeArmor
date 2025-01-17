package core

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/client"
	"golang.org/x/net/context"

	kl "github.com/kubearmor/KubeArmor/KubeArmor/common"
	tp "github.com/kubearmor/KubeArmor/KubeArmor/types"
)

// ==================== //
// == Docker Handler == //
// ==================== //

// Docker Handler
var Docker *DockerHandler

// init Function
func init() {
	Docker = NewDockerHandler()
}

// DockerVersion Structure
type DockerVersion struct {
	APIVersion string `json:"ApiVersion"`
}

// DockerHandler Structure
type DockerHandler struct {
	DockerClient *client.Client
	Version      DockerVersion
}

// NewDockerHandler Function
func NewDockerHandler() *DockerHandler {
	docker := &DockerHandler{}

	// specify the docker api version that we want to use
	// Versioned API: https://docs.docker.com/engine/api/

	versionStr, err := kl.GetCommandOutputWithErr("curl", []string{"--unix-socket", "/var/run/docker.sock", "http://localhost/version"})
	if err != nil {
		return nil
	}

	json.Unmarshal([]byte(versionStr), &docker.Version)
	apiVersion, _ := strconv.ParseFloat(docker.Version.APIVersion, 64)

	if apiVersion >= 1.39 {
		// downgrade the api version to 1.39
		os.Setenv("DOCKER_API_VERSION", "1.39")
	} else {
		// set the current api version
		os.Setenv("DOCKER_API_VERSION", docker.Version.APIVersion)
	}

	// create a new client with the above env variable

	DockerClient, err := client.NewEnvClient()
	if err != nil {
		return nil
	}
	docker.DockerClient = DockerClient

	return docker
}

// Close Function
func (dh *DockerHandler) Close() {
	if dh.DockerClient != nil {
		dh.DockerClient.Close()
	}
}

// ==================== //
// == Container Info == //
// ==================== //

// GetContainerInfo Function
func (dh *DockerHandler) GetContainerInfo(containerID string) (tp.Container, error) {
	if dh.DockerClient == nil {
		return tp.Container{}, errors.New("No docker client")
	}

	inspect, err := dh.DockerClient.ContainerInspect(context.Background(), containerID)
	if err != nil {
		return tp.Container{}, err
	}

	container := tp.Container{}

	// == container base == //

	container.ContainerID = inspect.ID
	container.ContainerName = strings.TrimLeft(inspect.Name, "/")

	container.NamespaceName = "Unknown"
	container.ContainerGroupName = "Unknown"

	containerLabels := inspect.Config.Labels
	if _, ok := containerLabels["io.kubernetes.pod.namespace"]; ok { // kubernetes
		if val, ok := containerLabels["io.kubernetes.pod.namespace"]; ok {
			container.NamespaceName = val
		}
		if val, ok := containerLabels["io.kubernetes.pod.name"]; ok {
			container.ContainerGroupName = val
		}
	}

	container.AppArmorProfile = inspect.AppArmorProfile

	// == //

	return container, nil
}

// ========================== //
// == Docker Event Channel == //
// ========================== //

// GetEventChannel Function
func (dh *DockerHandler) GetEventChannel() <-chan events.Message {
	if dh.DockerClient != nil {
		event, _ := dh.DockerClient.Events(context.Background(), types.EventsOptions{})
		return event
	}

	return nil
}

// =================== //
// == Docker Events == //
// =================== //

// GetAlreadyDeployedDockerContainers Function
func (dm *KubeArmorDaemon) GetAlreadyDeployedDockerContainers() {
	if containerList, err := Docker.DockerClient.ContainerList(context.Background(), types.ContainerListOptions{}); err == nil {
		for _, dcontainer := range containerList {
			// get container information from docker client
			container, err := Docker.GetContainerInfo(dcontainer.ID)
			if err != nil {
				continue
			}

			if container.ContainerID == "" {
				continue
			}

			if dcontainer.State == "running" {
				dm.ContainersLock.Lock()
				if _, ok := dm.Containers[container.ContainerID]; !ok {
					dm.Containers[container.ContainerID] = container
				} else if dm.Containers[container.ContainerID].AppArmorProfile == "" && container.AppArmorProfile != "" {
					dm.Containers[container.ContainerID] = container
				} else {
					dm.ContainersLock.Unlock()
					continue
				}
				dm.ContainersLock.Unlock()

				dm.LogFeeder.Printf("Detected a container (added/%s)", container.ContainerID[:12])
			}
		}
	}

	for _, conGroup := range dm.ContainerGroups {
		for _, containerID := range conGroup.Containers {
			if container, ok := dm.Containers[containerID]; ok {
				if conGroup.AppArmorProfiles[containerID] == "" {
					conGroup.AppArmorProfiles[containerID] = container.AppArmorProfile
				}
			}
		}
	}
}

// UpdateDockerContainer Function
func (dm *KubeArmorDaemon) UpdateDockerContainer(containerID, action string) {
	container := tp.Container{}

	if action == "start" {
		var err error

		// get container information from docker client
		container, err = Docker.GetContainerInfo(containerID)
		if err != nil {
			return
		}

		if container.ContainerID == "" {
			return
		}

		dm.ContainersLock.Lock()
		if _, ok := dm.Containers[containerID]; !ok {
			dm.Containers[containerID] = container
		} else {
			dm.ContainersLock.Unlock()
			return
		}
		dm.ContainersLock.Unlock()

		dm.LogFeeder.Printf("Detected a container (added/%s)", containerID[:12])

	} else if action == "stop" || action == "destroy" {
		// case 1: kill -> die -> stop
		// case 2: kill -> die -> destroy
		// case 3: destroy

		dm.ContainersLock.Lock()
		if val, ok := dm.Containers[containerID]; !ok {
			dm.ContainersLock.Unlock()
			return
		} else {
			container = val
			delete(dm.Containers, containerID)
		}
		dm.ContainersLock.Unlock()

		dm.LogFeeder.Printf("Detected a container (removed/%s)", containerID[:12])
	}
}

// MonitorDockerEvents Function
func (dm *KubeArmorDaemon) MonitorDockerEvents() {
	dm.WgDaemon.Add(1)
	defer dm.WgDaemon.Done()

	if Docker == nil {
		return
	}

	dm.LogFeeder.Print("Started to monitor Docker events")

	EventChan := Docker.GetEventChannel()

	for {
		select {
		case <-StopChan:
			return

		case msg, valid := <-EventChan:
			if !valid {
				continue
			}

			// if message type is container
			if msg.Type == "container" {
				dm.UpdateDockerContainer(msg.ID, msg.Action)
			}
		}
	}
}
