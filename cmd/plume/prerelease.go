// Copyright 2016 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	azurestorage "github.com/Azure/azure-sdk-for-go/arm/storage"
	"github.com/Microsoft/azure-vhd-utils/vhdcore/validator"
	"github.com/spf13/cobra"
	"golang.org/x/net/context"
	gs "google.golang.org/api/storage/v1"

	"github.com/coreos/mantle/platform/api/aws"
	"github.com/coreos/mantle/platform/api/azure"
	"github.com/coreos/mantle/sdk"
	"github.com/coreos/mantle/storage"
	"github.com/coreos/mantle/util"
)

var (
	cmdPreRelease = &cobra.Command{
		Use:   "pre-release [options]",
		Short: "Run pre-release steps for CoreOS",
		Long:  "Runs pre-release steps for CoreOS, such as image uploading and OS image creation, and replication across regions.",
		RunE:  runPreRelease,
	}

	platforms = map[string]platform{
		"aws": platform{
			displayName: "AWS",
			handler:     awsPreRelease,
		},
		"azure": platform{
			displayName: "Azure",
			handler:     azurePreRelease,
		},
	}
	platformList []string

	selectedPlatforms  []string
	selectedDistro     string
	force              bool
	azureProfile       string
	azureAuth          string
	azureTestContainer string
	awsCredentialsFile string
	verifyKeyFile      string
	imageInfoFile      string
)

type imageMetadataAbstract struct {
	Env       string
	Version   string
	Timestamp string
	Respin    string
	ImageType string
	Arch      string
}

type platform struct {
	displayName string
	handler     func(context.Context, *http.Client, *storage.Bucket, *channelSpec, *imageInfo) error
}

type imageInfo struct {
	AWS   *amiList        `json:"aws,omitempty"`
	Azure *azureImageInfo `json:"azure,omitempty"`
}

func init() {
	for k, _ := range platforms {
		platformList = append(platformList, k)
	}
	sort.Sort(sort.StringSlice(platformList))

	cmdPreRelease.Flags().StringSliceVar(&selectedPlatforms, "platform", platformList, "platform to pre-release")
	cmdPreRelease.Flags().StringVar(&selectedDistro, "system", "cl", "system to pre-release")
	cmdPreRelease.Flags().BoolVar(&force, "force", false, "Replace existing images")
	cmdPreRelease.Flags().StringVar(&azureProfile, "azure-profile", "", "Azure Profile json file")
	cmdPreRelease.Flags().StringVar(&azureAuth, "azure-auth", "", "Azure Credentials json file")
	cmdPreRelease.Flags().StringVar(&azureTestContainer, "azure-test-container", "", "Use test container instead of default")
	cmdPreRelease.Flags().StringVar(&awsCredentialsFile, "aws-credentials", "", "AWS credentials file")
	cmdPreRelease.Flags().StringVar(&verifyKeyFile,
		"verify-key", "", "path to ASCII-armored PGP public key to be used in verifying download signatures.  Defaults to CoreOS Buildbot (0412 7D0B FABE C887 1FFB  2CCE 50E0 8855 93D2 DCB4)")
	cmdPreRelease.Flags().StringVar(&imageInfoFile, "write-image-list", "", "optional output file describing uploaded images")

	AddSpecFlags(cmdPreRelease.Flags())
	AddFedoraSpecFlags(cmdPreRelease.Flags())
	root.AddCommand(cmdPreRelease)
}

func runPreRelease(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return errors.New("no args accepted")
	}

	for _, platformName := range selectedPlatforms {
		if _, ok := platforms[platformName]; !ok {
			return fmt.Errorf("Unknown platform %q", platformName)
		}
	}

	switch selectedDistro {
	case "cl":
		if err := runCLPreRelease(cmd); err != nil {
			return err
		}
	case "fedora":
		if err := runFedoraPreRelease(cmd); err != nil {
			return err
		}
	default:
		return fmt.Errorf("Unknown distro %q", selectedDistro)
	}
	plog.Printf("Pre-release complete, run `plume release` to finish.")

	return nil
}

func runFedoraPreRelease(cmd *cobra.Command) error {
	spec, err := ChannelFedoraSpec()
	if err != nil {
		return err
	}
	ctx := context.Background()
	client := http.Client{}

	var imageInfo imageInfo

	for _, platformName := range selectedPlatforms {
		platform := platforms[platformName]
		plog.Printf("Running %v pre-release...", platform.displayName)
		if err := platform.handler(ctx, &client, nil, &spec, &imageInfo); err != nil {
			return err
		}
	}

	return nil
}

func runCLPreRelease(cmd *cobra.Command) error {
	spec := ChannelSpec()
	ctx := context.Background()
	client, err := getGoogleClient()
	if err != nil {
		plog.Fatal(err)
	}

	src, err := storage.NewBucket(client, spec.SourceURL())
	if err != nil {
		plog.Fatal(err)
	}

	if err := src.Fetch(ctx); err != nil {
		plog.Fatal(err)
	}

	// Sanity check!
	if vertxt := src.Object(src.Prefix() + "version.txt"); vertxt == nil {
		verurl := src.URL().String() + "version.txt"
		plog.Fatalf("File not found: %s", verurl)
	}

	var imageInfo imageInfo
	for _, platformName := range selectedPlatforms {
		platform := platforms[platformName]
		plog.Printf("Running %v pre-release...", platform.displayName)
		if err := platform.handler(ctx, client, src, &spec, &imageInfo); err != nil {
			plog.Fatal(err)
		}
	}

	if imageInfoFile != "" {
		f, err := os.OpenFile(imageInfoFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0666)
		if err != nil {
			plog.Fatal(err)
		}
		defer f.Close()

		encoder := json.NewEncoder(f)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(imageInfo); err != nil {
			plog.Fatalf("couldn't encode image list: %v", err)
		}
	}

	return nil
}

// getImageFile downloads a bzipped CoreOS image, verifies its signature,
// decompresses it, and returns the decompressed path.
func getImageFile(client *http.Client, spec *channelSpec, src *storage.Bucket, fileName string) (string, error) {
	switch selectedDistro {
	case "cl":
		return getCLImageFile(client, src, fileName)
	case "fedora":
		return getFedoraImageFile(client, spec, src, fileName)
	default:
		return "", fmt.Errorf("Invalid system: %v", selectedDistro)
	}
}

func getImageTypeURI() string {
	if specImageType == "Cloud-Base" {
		return "Cloud"
	}
	return specImageType
}

func getCLImageFile(client *http.Client, src *storage.Bucket, fileName string) (string, error) {
	cacheDir := filepath.Join(sdk.RepoCache(), "images", specChannel, specBoard, specVersion)
	bzipPath := filepath.Join(cacheDir, fileName)
	imagePath := strings.TrimSuffix(bzipPath, filepath.Ext(bzipPath))

	if _, err := os.Stat(imagePath); err == nil {
		if !force {
			plog.Printf("Reusing existing image %q", imagePath)
			return imagePath, nil
		} else {
			if err := os.Remove(imagePath); err != nil {
				return "", err
			}
		}
	}

	bzipUri, err := url.Parse(fileName)
	if err != nil {
		return "", err
	}

	bzipUri = src.URL().ResolveReference(bzipUri)

	plog.Printf("Downloading image %q to %q", bzipUri, bzipPath)

	if err := sdk.UpdateSignedFile(bzipPath, bzipUri.String(), client, verifyKeyFile); err != nil {
		return "", err
	}

	// decompress it
	plog.Printf("Decompressing %q...", bzipPath)
	if err := util.Bunzip2File(imagePath, bzipPath); err != nil {
		return "", err
	}
	return imagePath, nil
}

func getFedoraImageFile(client *http.Client, spec *channelSpec, src *storage.Bucket, fileName string) (string, error) {
	cacheDir := filepath.Join(sdk.RepoCache(), "images", specChannel, specVersion)
	rawxzPath := filepath.Join(cacheDir, fileName)
	imagePath := strings.TrimSuffix(rawxzPath, ".xz")

	if _, err := os.Stat(imagePath); err == nil {
		plog.Printf("Reusing existing image %q", imagePath)
		return imagePath, nil
	}

	rawxzURI, err := url.Parse(fmt.Sprintf("%v/%v/compose/%v/%v/images/%v", spec.BaseURL, specComposeID, getImageTypeURI(), specBoard, fileName))
	if err != nil {
		return "", err
	}

	plog.Printf("Downloading image %q to %q", rawxzURI, rawxzPath)

	if err := sdk.UpdateFile(rawxzPath, rawxzURI.String(), client); err != nil {
		return "", err
	}

	// decompress it
	plog.Printf("Decompressing %q...", rawxzPath)
	if err := util.XZ2File(imagePath, rawxzPath); err != nil {
		return "", err
	}
	return imagePath, nil
}

func uploadAzureBlob(spec *channelSpec, api *azure.API, storageKeys azurestorage.AccountListKeysResult, vhdfile, container, blobName string) error {
	for _, key := range *storageKeys.Keys {
		blobExists, err := api.BlobExists(spec.Azure.StorageAccount, *key.Value, container, blobName)
		if err != nil {
			return fmt.Errorf("failed to check if file %q in account %q container %q exists: %v", vhdfile, spec.Azure.StorageAccount, container, err)
		}

		if blobExists {
			if !force {
				return nil
			} else {
				if err := api.DeleteBlob(spec.Azure.StorageAccount, *key.Value, container, blobName); err != nil {
					return err
				}
			}
		}

		if err := api.UploadBlob(spec.Azure.StorageAccount, *key.Value, vhdfile, container, blobName, false); err != nil {
			if _, ok := err.(azure.BlobExistsError); !ok {
				return fmt.Errorf("uploading file %q to account %q container %q failed: %v", vhdfile, spec.Azure.StorageAccount, container, err)
			}
		}
		break
	}
	return nil
}

func createAzureImage(spec *channelSpec, api *azure.API, blobName, imageName string) error {
	// If it exists already the API will behave as if it created it
	plog.Printf("Creating OS image with name %q", imageName)

	bloburl := api.UrlOfBlob(spec.Azure.StorageAccount, spec.Azure.Container, blobName).String()

	img, err := api.CreateImage(imageName, spec.Azure.ResourceGroup, bloburl)
	if err != nil {
		return fmt.Errorf("Couldn't create image %q from %q: %T %v", imageName, bloburl, err, err)
	}
	if img.ID == nil {
		return fmt.Errorf("Couldn't create image %q form %q: received nil image", imageName, bloburl)
	}
	return nil
}

func replicateAzureImage(spec *channelSpec, api *azure.API, imageName string) error {
	plog.Printf("Fetching Azure Locations...")
	locations, err := api.Locations()
	if err != nil {
		return err
	}

	plog.Printf("Replicating image to locations: %s", strings.Join(locations, ", "))

	channelTitle := strings.Title(specChannel)

	if err := api.ReplicateImage(imageName, spec.Azure.Offer, channelTitle, specVersion, locations...); err != nil {
		return fmt.Errorf("image replication failed: %v", err)
	}

	return nil
}

type azureImageInfo struct {
	ImageName string `json:"image"`
}

// azurePreRelease runs everything necessary to prepare a CoreOS release for Azure.
//
// This includes uploading the vhd image to Azure storage, creating an OS image from it,
// and replicating that OS image.
func azurePreRelease(ctx context.Context, client *http.Client, src *storage.Bucket, spec *channelSpec, imageInfo *imageInfo) error {
	if spec.Azure.StorageAccount == "" {
		plog.Notice("Azure image creation disabled.")
		return nil
	}

	// download azure vhd image and unzip it
	vhdfile, err := getImageFile(client, spec, src, spec.Azure.Image)
	if err != nil {
		return err
	}

	// sanity check - validate VHD file
	plog.Printf("Validating VHD file %q", vhdfile)
	if err := validator.ValidateVhd(vhdfile); err != nil {
		return err
	}
	if err := validator.ValidateVhdSize(vhdfile); err != nil {
		return err
	}

	blobName := fmt.Sprintf("flatcar-linux-%s-%s.vhd", specVersion, specChannel)

	for _, environment := range spec.Azure.Environments {
		// construct azure api client
		api, err := azure.New(&azure.Options{
			AzureProfile:      azureProfile,
			AzureAuthLocation: azureAuth,
			AzureSubscription: environment.SubscriptionName,
		})
		if err != nil {
			return fmt.Errorf("failed to create Azure API: %v", err)
		}
		if err := api.SetupClients(); err != nil {
			return fmt.Errorf("setting up clients: %v", err)
		}

		plog.Printf("Fetching Azure storage credentials")

		storageKey, err := api.GetStorageServiceKeysARM(spec.Azure.StorageAccount, spec.Azure.ResourceGroup)
		if err != nil {
			return err
		}
		if storageKey.Keys == nil {
			plog.Fatalf("No storage service keys found")
		}

		// upload blob, do not overwrite
		plog.Printf("Uploading %q to Azure Storage...", vhdfile)

		container := spec.Azure.Container
		if azureTestContainer != "" {
			container = azureTestContainer
		}
		err = uploadAzureBlob(spec, api, storageKey, vhdfile, container, blobName)
		if err != nil {
			return err
		}
		var sas string
		for _, key := range *storageKey.Keys {
			sas, err = api.SignBlob(spec.Azure.StorageAccount, *key.Value, container, blobName)
			if err == nil {
				break
			}
		}
		if err != nil {
			plog.Fatalf("signing failed: %v", err)
		}
		url := api.UrlOfBlob(spec.Azure.StorageAccount, container, blobName).String()
		plog.Noticef("Generated SAS: %q from %q for %q", sas, url, specChannel)
		imageInfo.Azure = &azureImageInfo{
			ImageName: sas, // the SAS URL can be used for publishing and for testing with kola via --azure-blob-url
		}
	}
	return nil
}

func getSpecAWSImageMetadata(spec *channelSpec) (map[string]string, error) {
	imageFileName := spec.AWS.Image
	imageMetadata := imageMetadataAbstract{
		Env:       specEnv,
		Version:   specVersion,
		Timestamp: specTimestamp,
		Respin:    specRespin,
		ImageType: specImageType,
		Arch:      specBoard,
	}
	t := template.Must(template.New("filename").Parse(imageFileName))
	buffer := &bytes.Buffer{}
	if err := t.Execute(buffer, imageMetadata); err != nil {
		return nil, err
	}
	imageFileName = buffer.String()

	var imageName string
	switch selectedDistro {
	case "cl":
		imageName = fmt.Sprintf("%v-%v-%v%v", spec.AWS.BaseName, specChannel, specVersion, AmiNameArchTag())
		imageName = regexp.MustCompile(`[^A-Za-z0-9()\\./_-]`).ReplaceAllLiteralString(imageName, "_")
	case "fedora":
		imageName = strings.TrimSuffix(imageFileName, ".raw.xz")
	}

	imageDescription := fmt.Sprintf("%v %v %v%v", spec.AWS.BaseDescription, specChannel, specVersion, strings.ReplaceAll(AmiNameArchTag(), "-", " "))

	awsImageMetaData := map[string]string{
		"imageFileName":    imageFileName,
		"imageName":        imageName,
		"imageDescription": imageDescription,
	}

	return awsImageMetaData, nil
}

func awsUploadToPartition(spec *channelSpec, part *awsPartitionSpec, imagePath string) (map[string]string, error) {
	plog.Printf("Connecting to %v...", part.Name)
	api, err := aws.New(&aws.Options{
		CredentialsFile: awsCredentialsFile,
		Profile:         part.Profile,
		Region:          part.BucketRegion,
	})
	if err != nil {
		return nil, fmt.Errorf("creating client for %v: %v", part.Name, err)
	}

	f, err := os.Open(imagePath)
	if err != nil {
		return nil, fmt.Errorf("Could not open image file %v: %v", imagePath, err)
	}
	defer f.Close()

	awsImageMetadata, err := getSpecAWSImageMetadata(spec)
	if err != nil {
		return nil, fmt.Errorf("Could not generate the image metadata: %v", err)
	}

	imageFileName := awsImageMetadata["imageFileName"]
	imageName := awsImageMetadata["imageName"]
	imageDescription := awsImageMetadata["imageDescription"]

	var s3ObjectPath string
	switch selectedDistro {
	case "cl":
		s3ObjectPath = fmt.Sprintf("%s/%s/%s", specBoard, specVersion, strings.TrimSuffix(imageFileName, filepath.Ext(imageFileName)))
	case "fedora":
		s3ObjectPath = fmt.Sprintf("%s/%s/%s", specBoard, specVersion, strings.TrimSuffix(imageFileName, filepath.Ext(imageFileName)))
	}
	s3ObjectURL := fmt.Sprintf("s3://%s/%s", part.Bucket, s3ObjectPath)

	destRegions := make([]string, 0, len(part.Regions))
	foundBucketRegion := false
	for _, region := range part.Regions {
		if region != part.BucketRegion {
			destRegions = append(destRegions, region)
		} else {
			foundBucketRegion = true
		}
	}
	if !foundBucketRegion {
		// We don't handle this case and shouldn't ever
		// encounter it
		return nil, fmt.Errorf("BucketRegion %v is not listed in Regions", part.BucketRegion)
	}

	if force {
		err := api.RemoveImage(imageName, part.Bucket, s3ObjectPath, destRegions)
		if err != nil {
			return nil, err
		}
	}

	snapshot, err := api.FindSnapshot(imageName)
	if err != nil {
		return nil, fmt.Errorf("unable to check for snapshot: %v", err)
	}

	if snapshot == nil {
		plog.Printf("Creating S3 object %v...", s3ObjectURL)
		err = api.UploadObject(f, part.Bucket, s3ObjectPath, false)
		if err != nil {
			return nil, fmt.Errorf("Error uploading: %v", err)
		}

		plog.Printf("Creating EBS snapshot...")

		var format aws.EC2ImageFormat
		switch selectedDistro {
		case "cl":
			format = aws.EC2ImageFormatVmdk
		case "fedora":
			format = aws.EC2ImageFormatRaw
		}

		snapshot, err = api.CreateSnapshot(imageName, s3ObjectURL, format)
		if err != nil {
			return nil, fmt.Errorf("unable to create snapshot: %v", err)
		}
	}

	// delete unconditionally to avoid leaks after a restart
	plog.Printf("Deleting S3 object %v...", s3ObjectURL)
	err = api.DeleteObject(part.Bucket, s3ObjectPath)
	if err != nil {
		return nil, fmt.Errorf("Error deleting S3 object: %v", err)
	}

	plog.Printf("Creating AMIs from %v...", snapshot.SnapshotID)

	amiArch, err := aws.AmiArchForBoard(specBoard)
	if err != nil {
		return nil, fmt.Errorf("could not get architecture for board: %v", err)
	}

	hvmImageID, err := api.CreateHVMImage(snapshot.SnapshotID, aws.ContainerLinuxDiskSizeGiB, imageName+"-hvm", imageDescription+" (HVM)", amiArch)
	if err != nil {
		return nil, fmt.Errorf("unable to create HVM image: %v", err)
	}
	resources := []string{snapshot.SnapshotID, hvmImageID}

	switch selectedDistro {
	case "cl":
		err = api.CreateTags(resources, map[string]string{
			"Channel": specChannel,
			"Version": specVersion,
		})
		if err != nil {
			return nil, fmt.Errorf("couldn't tag images: %v", err)
		}
	case "fedora":
		err = api.CreateTags(resources, map[string]string{
			"Channel":   specChannel,
			"Version":   specVersion,
			"ComposeID": specComposeID,
		})
		if err != nil {
			return nil, fmt.Errorf("couldn't tag images: %v", err)
		}
	}

	postprocess := func(imageID string) (map[string]string, error) {
		if len(part.LaunchPermissions) > 0 {
			if err := api.GrantLaunchPermission(imageID, part.LaunchPermissions); err != nil {
				return nil, err
			}
		}

		amis := map[string]string{}
		if len(destRegions) > 0 {
			plog.Printf("Replicating AMI %v...", imageID)
			amis, err = api.CopyImage(imageID, destRegions)
			if err != nil {
				return nil, fmt.Errorf("couldn't copy image: %v", err)
			}
		}
		amis[part.BucketRegion] = imageID

		return amis, nil
	}

	hvmAmis, err := postprocess(hvmImageID)
	if err != nil {
		return nil, fmt.Errorf("processing HVM images: %v", err)
	}

	return hvmAmis, nil
}

type amiListEntry struct {
	Region string `json:"name"`
	HvmAmi string `json:"hvm"`
}

type amiList struct {
	Entries []amiListEntry `json:"amis"`
}

func awsUploadAmiLists(ctx context.Context, bucket *storage.Bucket, spec *channelSpec, amis *amiList) error {
	upload := func(name string, data string) error {
		var contentType string
		if strings.HasSuffix(name, ".txt") {
			contentType = "text/plain"
		} else if strings.HasSuffix(name, ".json") {
			contentType = "application/json"
		} else {
			return fmt.Errorf("unknown file extension in %v", name)
		}

		obj := gs.Object{
			Name:        bucket.Prefix() + spec.AWS.Prefix + name,
			ContentType: contentType,
		}
		media := bytes.NewReader([]byte(data))
		if err := bucket.Upload(ctx, &obj, media); err != nil {
			return fmt.Errorf("couldn't upload %v: %v", name, err)
		}
		return nil
	}

	// emit keys in stable order
	sort.Slice(amis.Entries, func(i, j int) bool {
		return amis.Entries[i].Region < amis.Entries[j].Region
	})

	// format JSON AMI list
	var jsonBuf bytes.Buffer
	encoder := json.NewEncoder(&jsonBuf)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(amis); err != nil {
		return fmt.Errorf("couldn't encode JSON: %v", err)
	}
	jsonAll := jsonBuf.String()

	// format text AMI lists and upload AMI IDs for individual regions
	var hvmRecords []string
	for _, entry := range amis.Entries {
		hvmRecords = append(hvmRecords,
			fmt.Sprintf("%v=%v", entry.Region, entry.HvmAmi))

		if err := upload(fmt.Sprintf("hvm_%v.txt", entry.Region),
			entry.HvmAmi+"\n"); err != nil {
			return err
		}
		if err := upload(fmt.Sprintf("%v.txt", entry.Region),
			entry.HvmAmi+"\n"); err != nil {
			return err
		}
	}
	hvmAll := strings.Join(hvmRecords, "|") + "\n"

	// upload AMI lists
	if err := upload("all.json", jsonAll); err != nil {
		return err
	}
	if err := upload("hvm.txt", hvmAll); err != nil {
		return err
	}
	// compatibility
	if err := upload("all.txt", hvmAll); err != nil {
		return err
	}

	return nil
}

// awsPreRelease runs everything necessary to prepare a CoreOS release for AWS.
//
// This includes uploading the ami_vmdk image to an S3 bucket in each EC2
// partition, creating HVM AMIs, and replicating the AMIs to each
// region.
func awsPreRelease(ctx context.Context, client *http.Client, src *storage.Bucket, spec *channelSpec, imageInfo *imageInfo) error {
	if spec.AWS.Image == "" {
		plog.Notice("AWS image creation disabled.")
		return nil
	}

	awsImageMetadata, err := getSpecAWSImageMetadata(spec)
	if err != nil {
		return fmt.Errorf("Could not generate the image filname: %v", err)
	}

	imageFileName := awsImageMetadata["imageFileName"]

	imagePath, err := getImageFile(client, spec, src, imageFileName)
	if err != nil {
		return err
	}

	var amis amiList
	for i := range spec.AWS.Partitions {
		hvmAmis, err := awsUploadToPartition(spec, &spec.AWS.Partitions[i], imagePath)
		if err != nil {
			return err
		}

		for region := range hvmAmis {
			amis.Entries = append(amis.Entries, amiListEntry{
				Region: region,
				HvmAmi: hvmAmis[region],
			})
		}
	}

	if selectedDistro == "cl" && gceJSONKeyFile != "none" {
		if err := awsUploadAmiLists(ctx, src, spec, &amis); err != nil {
			return fmt.Errorf("uploading AMI IDs: %v", err)
		}
	}

	imageInfo.AWS = &amis
	return nil
}
