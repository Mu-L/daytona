// Copyright 2024 Daytona Platforms Inc.
// SPDX-License-Identifier: Apache-2.0

package builder

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/daytonaio/daytona/pkg/git"
	"github.com/daytonaio/daytona/pkg/gitprovider"
	"github.com/daytonaio/daytona/pkg/logger"
	"github.com/daytonaio/daytona/pkg/ports"
	"github.com/daytonaio/daytona/pkg/workspace"
	"github.com/docker/docker/pkg/stringid"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

type IBuilderFactory interface {
	Create(p workspace.Project, gpc *gitprovider.GitProviderConfig) (IBuilder, error)
	CheckExistingBuild(p workspace.Project) (*BuildResult, error)
}

type BuilderFactory struct {
	daytonaServerConfigFolder       string
	localContainerRegistryServer    string
	basePath                        string
	loggerFactory                   logger.LoggerFactory
	defaultProjectImage             string
	defaultProjectUser              string
	defaultProjectPostStartCommands []string
}

func NewBuilderFactory(config BuilderConfig) IBuilderFactory {
	return &BuilderFactory{
		daytonaServerConfigFolder:       config.DaytonaServerConfigFolder,
		localContainerRegistryServer:    config.LocalContainerRegistryServer,
		basePath:                        config.BasePath,
		loggerFactory:                   config.LoggerFactory,
		defaultProjectImage:             config.DefaultProjectImage,
		defaultProjectUser:              config.DefaultProjectUser,
		defaultProjectPostStartCommands: config.DefaultProjectPostStartCommands,
	}
}

func (f *BuilderFactory) Create(p workspace.Project, gpc *gitprovider.GitProviderConfig) (IBuilder, error) {
	buildId := stringid.GenerateRandomID()
	buildId = stringid.TruncateID(buildId)

	hash, err := p.GetConfigHash()
	if err != nil {
		return nil, err
	}
	projectDir := filepath.Join(f.basePath, hash, "project")

	err = os.RemoveAll(projectDir)
	if err != nil {
		return nil, err
	}

	projectLogger := f.loggerFactory.CreateProjectLogger(p.WorkspaceId, p.Name)
	defer projectLogger.Close()

	gitservice := git.Service{
		ProjectDir:        projectDir,
		GitConfigFileName: "",
		LogWriter:         projectLogger,
	}

	var auth *http.BasicAuth
	if gpc != nil {
		auth = &http.BasicAuth{
			Username: gpc.Username,
			Password: gpc.Token,
		}
	}

	err = gitservice.CloneRepository(&p, auth)
	if err != nil {
		return nil, err
	}

	buildConfig := p.Build

	if buildConfig == nil || *buildConfig != (workspace.ProjectBuild{}) {
		return nil, nil
	}

	devcontainerConfigFilePath, err := detectDevcontainerConfigFilePath(projectDir)
	if err != nil {
		return nil, err
	}
	if devcontainerConfigFilePath != "" {
		buildConfig.Devcontainer = &workspace.ProjectBuildDevcontainer{
			DevContainerFilePath: devcontainerConfigFilePath,
		}

		builderDockerPort, err := ports.GetAvailableEphemeralPort()
		if err != nil {
			return nil, err
		}

		return &DevcontainerBuilder{
			Builder: &Builder{
				id:                              buildId,
				project:                         p,
				gitProviderConfig:               gpc,
				hash:                            hash,
				projectVolumePath:               projectDir,
				daytonaServerConfigFolder:       f.daytonaServerConfigFolder,
				localContainerRegistryServer:    f.localContainerRegistryServer,
				basePath:                        f.basePath,
				loggerFactory:                   f.loggerFactory,
				defaultProjectImage:             f.defaultProjectImage,
				defaultProjectUser:              f.defaultProjectUser,
				defaultProjectPostStartCommands: f.defaultProjectPostStartCommands,
			},
			builderDockerPort: builderDockerPort,
		}, nil
	}

	return nil, nil
}

func (f *BuilderFactory) CheckExistingBuild(p workspace.Project) (*BuildResult, error) {
	hash, err := p.GetConfigHash()
	if err != nil {
		return nil, err
	}

	filePath := filepath.Join(f.daytonaServerConfigFolder, "builds", hash, "build.json")

	_, err = os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var result BuildResult
	decoder := json.NewDecoder(file)
	err = decoder.Decode(&result)
	if err != nil {
		return nil, err
	}

	return &result, nil
}

func fileExists(filePath string) (bool, error) {
	_, err := os.Stat(filePath)
	if os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		// There was an error checking for the file
		return false, err
	}
	return true, nil
}

func detectDevcontainerConfigFilePath(projectDir string) (string, error) {
	devcontainerPath := ".devcontainer/devcontainer.json"
	isDevcontainer, err := fileExists(filepath.Join(projectDir, devcontainerPath))
	if err != nil {
		devcontainerPath = ".devcontainer.json"
		isDevcontainer, err = fileExists(filepath.Join(projectDir, devcontainerPath))
		if err != nil {
			return devcontainerPath, nil
		}
	}
	if isDevcontainer {
		return devcontainerPath, nil
	} else {
		return "", nil
	}
}