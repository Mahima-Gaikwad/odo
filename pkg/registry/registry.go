package registry

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"

	devfilev1 "github.com/devfile/api/v2/pkg/apis/workspaces/v1alpha2"
	dfutil "github.com/devfile/library/pkg/util"
	indexSchema "github.com/devfile/registry-support/index/generator/schema"
	"github.com/devfile/registry-support/registry-library/library"
	registryUtil "github.com/redhat-developer/odo/pkg/odo/cli/preference/registry/util"
	"github.com/zalando/go-keyring"

	"github.com/redhat-developer/odo/pkg/component"
	"github.com/redhat-developer/odo/pkg/log"
	"github.com/redhat-developer/odo/pkg/preference"
	"github.com/redhat-developer/odo/pkg/segment"
	"github.com/redhat-developer/odo/pkg/testingutil/filesystem"
	"github.com/redhat-developer/odo/pkg/util"
)

type RegistryClient struct {
	fsys             filesystem.Filesystem
	preferenceClient preference.Client
}

func NewRegistryClient(fsys filesystem.Filesystem, preferenceClient preference.Client) RegistryClient {
	return RegistryClient{
		fsys:             fsys,
		preferenceClient: preferenceClient,
	}
}

// PullStackFromRegistry pulls stack from registry with all stack resources (all media types) to the destination directory
func (o RegistryClient) PullStackFromRegistry(registry string, stack string, destDir string, options library.RegistryOptions) error {
	return library.PullStackFromRegistry(registry, stack, destDir, options)
}

// DownloadFileInMemory uses the url to download the file and return bytes
func (o RegistryClient) DownloadFileInMemory(params dfutil.HTTPRequestParams) ([]byte, error) {
	return util.DownloadFileInMemory(params)
}

// DownloadStarterProject downloads a starter project referenced in devfile
// This will first remove the content of the contextDir
func (o RegistryClient) DownloadStarterProject(starterProject *devfilev1.StarterProject, decryptedToken string, contextDir string, verbose bool) error {
	return component.DownloadStarterProject(starterProject, decryptedToken, contextDir, verbose)
}

// GetDevfileRegistries gets devfile registries from preference file,
// if registry name is specified return the specific registry, otherwise return all registries
func (o RegistryClient) GetDevfileRegistries(registryName string) ([]Registry, error) {
	var devfileRegistries []Registry

	hasName := len(registryName) != 0
	if o.preferenceClient.RegistryList() != nil {
		registryList := *o.preferenceClient.RegistryList()
		// Loop backwards here to ensure the registry display order is correct (display latest newly added registry firstly)
		for i := len(registryList) - 1; i >= 0; i-- {
			registry := registryList[i]
			if hasName {
				if registryName == registry.Name {
					reg := Registry{
						Name:   registry.Name,
						URL:    registry.URL,
						Secure: registry.Secure,
					}
					devfileRegistries = append(devfileRegistries, reg)
					return devfileRegistries, nil
				}
			} else {
				reg := Registry{
					Name:   registry.Name,
					URL:    registry.URL,
					Secure: registry.Secure,
				}
				devfileRegistries = append(devfileRegistries, reg)
			}
		}
	} else {
		return nil, nil
	}

	return devfileRegistries, nil
}

// ListDevfileStacks lists all the available devfile stacks in devfile registry
func (o RegistryClient) ListDevfileStacks(registryName string) (DevfileStackList, error) {
	catalogDevfileList := &DevfileStackList{}
	var err error

	// TODO: consider caching registry information for better performance since it should be fairly stable over time
	// Get devfile registries
	catalogDevfileList.DevfileRegistries, err = o.GetDevfileRegistries(registryName)
	if err != nil {
		return *catalogDevfileList, err
	}
	if catalogDevfileList.DevfileRegistries == nil {
		return *catalogDevfileList, nil
	}

	// first retrieve the indices for each registry, concurrently
	devfileIndicesMutex := &sync.Mutex{}
	retrieveRegistryIndices := util.NewConcurrentTasks(len(catalogDevfileList.DevfileRegistries))

	// The 2D slice index is the priority of the registry (highest priority has highest index)
	// and the element is the devfile slice that belongs to the registry
	registrySlice := make([][]DevfileStack, len(catalogDevfileList.DevfileRegistries))
	for regPriority, reg := range catalogDevfileList.DevfileRegistries {
		// Load the devfile registry index.json
		registry := reg                 // Needed to prevent the lambda from capturing the value
		registryPriority := regPriority // Needed to prevent the lambda from capturing the value
		retrieveRegistryIndices.Add(util.ConcurrentTask{ToRun: func(errChannel chan error) {
			registryDevfiles, err := getRegistryStacks(o.preferenceClient, registry)
			if err != nil {
				log.Warningf("Registry %s is not set up properly with error: %v, please check the registry URL and credential (refer `odo registry update --help`)\n", registry.Name, err)
				return
			}

			devfileIndicesMutex.Lock()
			registrySlice[registryPriority] = registryDevfiles
			devfileIndicesMutex.Unlock()
		}})
	}
	if err := retrieveRegistryIndices.Run(); err != nil {
		return *catalogDevfileList, err
	}

	for _, registryDevfiles := range registrySlice {
		catalogDevfileList.Items = append(catalogDevfileList.Items, registryDevfiles...)
	}

	return *catalogDevfileList, nil
}

// convertURL converts GitHub regular URL to GitHub raw URL, do nothing if the URL is not GitHub URL
// For example:
// GitHub regular URL: https://github.com/elsony/devfile-registry/tree/johnmcollier-crw
// GitHub raw URL: https://raw.githubusercontent.com/elsony/devfile-registry/johnmcollier-crw
func convertURL(URL string) (string, error) {
	url, err := url.Parse(URL)
	if err != nil {
		return "", err
	}

	if strings.Contains(url.Host, "github") && !strings.Contains(url.Host, "raw") {
		// Convert path part of the URL
		URLSlice := strings.Split(URL, "/")
		if len(URLSlice) > 2 && URLSlice[len(URLSlice)-2] == "tree" {
			// GitHub raw URL doesn't have "tree" structure in the URL, need to remove it
			URL = strings.Replace(URL, "/tree", "", 1)
		} else {
			// Add "master" branch for GitHub raw URL by default if branch is not specified
			URL = URL + "/master"
		}

		// Convert host part of the URL
		if url.Host == "github.com" {
			URL = strings.Replace(URL, "github.com", "raw.githubusercontent.com", 1)
		} else {
			URL = strings.Replace(URL, url.Host, "raw."+url.Host, 1)
		}
	}

	return URL, nil
}

const indexPath = "/devfiles/index.json"

// getRegistryStacks retrieves the registry's index devfile stack entries
func getRegistryStacks(preferenceClient preference.Client, registry Registry) ([]DevfileStack, error) {
	if !strings.Contains(registry.URL, "github") {
		// OCI-based registry
		devfileIndex, err := library.GetRegistryIndex(registry.URL, segment.GetRegistryOptions(), indexSchema.StackDevfileType)
		if err != nil {
			return nil, err
		}
		return createRegistryDevfiles(registry, devfileIndex)
	}
	// Github-based registry
	URL, err := convertURL(registry.URL)
	if err != nil {
		return nil, fmt.Errorf("unable to convert URL %s: %w", registry.URL, err)
	}
	registry.URL = URL
	indexLink := registry.URL + indexPath
	request := dfutil.HTTPRequestParams{
		URL: indexLink,
	}

	secure := registryUtil.IsSecure(preferenceClient, registry.Name)
	if secure {
		token, e := keyring.Get(fmt.Sprintf("%s%s", dfutil.CredentialPrefix, registry.Name), registryUtil.RegistryUser)
		if e != nil {
			return nil, fmt.Errorf("unable to get secure registry credential from keyring: %w", e)
		}
		request.Token = token
	}

	jsonBytes, err := dfutil.HTTPGetRequest(request, preferenceClient.GetRegistryCacheTime())
	if err != nil {
		return nil, fmt.Errorf("unable to download the devfile index.json from %s: %w", indexLink, err)
	}

	var devfileIndex []indexSchema.Schema
	err = json.Unmarshal(jsonBytes, &devfileIndex)
	if err != nil {
		if err := util.CleanDefaultHTTPCacheDir(); err != nil {
			log.Warning("Error while cleaning up cache dir.")
		}
		// we try once again
		jsonBytes, err := dfutil.HTTPGetRequest(request, preferenceClient.GetRegistryCacheTime())
		if err != nil {
			return nil, fmt.Errorf("unable to download the devfile index.json from %s: %w", indexLink, err)
		}

		err = json.Unmarshal(jsonBytes, &devfileIndex)
		if err != nil {
			return nil, fmt.Errorf("unable to unmarshal the devfile index.json from %s: %w", indexLink, err)
		}
	}
	return createRegistryDevfiles(registry, devfileIndex)
}

func createRegistryDevfiles(registry Registry, devfileIndex []indexSchema.Schema) ([]DevfileStack, error) {
	registryDevfiles := make([]DevfileStack, 0, len(devfileIndex))
	for _, devfileIndexEntry := range devfileIndex {
		stackDevfile := DevfileStack{
			Name:        devfileIndexEntry.Name,
			DisplayName: devfileIndexEntry.DisplayName,
			Description: devfileIndexEntry.Description,
			Link:        devfileIndexEntry.Links["self"],
			Registry:    registry,
			Language:    devfileIndexEntry.Language,
			Tags:        devfileIndexEntry.Tags,
			ProjectType: devfileIndexEntry.ProjectType,
		}
		registryDevfiles = append(registryDevfiles, stackDevfile)
	}

	return registryDevfiles, nil
}
