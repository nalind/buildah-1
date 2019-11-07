package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	buildahcli "github.com/containers/buildah/pkg/cli"
	"github.com/containers/buildah/pkg/formats"
	"github.com/containers/buildah/pkg/parse"
	"github.com/containers/image/v5/docker/reference"
	is "github.com/containers/image/v5/storage"
	"github.com/containers/image/v5/types"
	"github.com/containers/storage"
	units "github.com/docker/go-units"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

type jsonImage struct {
	ID           string    `json:"id"`
	Names        []string  `json:"names,omitempty"`
	Digest       string    `json:"digest,omitempty"`
	Digests      []string  `json:"digests,omitempty"`
	CreatedAt    string    `json:"createdat"`
	Size         string    `json:"size"`
	CreatedAtRaw time.Time `json:"createdatraw"`
	ReadOnly     bool      `json:"readonly"`
}

type imageOutputParams struct {
	ID           string
	Name         string
	Tag          string
	Digest       string
	Digests      []string
	CreatedAt    string
	Size         string
	CreatedAtRaw time.Time
	ReadOnly     bool
}

type imageOptions struct {
	all       bool
	digests   bool
	format    string
	json      bool
	noHeading bool
	truncate  bool
	quiet     bool
	readOnly  bool
}

type filterParams struct {
	dangling         string
	label            string
	beforeImage      string
	sinceImage       string
	beforeDate       time.Time
	sinceDate        time.Time
	referencePattern string
	readOnly         string
}

type imageResults struct {
	imageOptions
	filter string
}

var imagesHeader = map[string]string{
	"Name":      "REPOSITORY",
	"Tag":       "TAG",
	"Digest":    "DIGEST",
	"ID":        "IMAGE ID",
	"CreatedAt": "CREATED",
	"Size":      "SIZE",
	"ReadOnly":  "R/O",
}

func init() {
	var (
		opts              imageResults
		imagesDescription = "\n  Lists locally stored images."
	)
	imagesCommand := &cobra.Command{
		Use:   "images",
		Short: "List images in local storage",
		Long:  imagesDescription,
		RunE: func(cmd *cobra.Command, args []string) error {
			return imagesCmd(cmd, args, &opts)
		},
		Example: `buildah images --all
  buildah images [imageName]
  buildah images --format '{{.ID}} {{.Name}} {{.Size}} {{.CreatedAtRaw}}'`,
	}
	imagesCommand.SetUsageTemplate(UsageTemplate())

	flags := imagesCommand.Flags()
	flags.SetInterspersed(false)
	flags.BoolVarP(&opts.all, "all", "a", false, "show all images, including intermediate images from a build")
	flags.BoolVar(&opts.digests, "digests", false, "show digests")
	flags.StringVarP(&opts.filter, "filter", "f", "", "filter output based on conditions provided")
	flags.StringVar(&opts.format, "format", "", "pretty-print images using a Go template")
	flags.BoolVar(&opts.json, "json", false, "output in JSON format")
	flags.BoolVarP(&opts.noHeading, "noheading", "n", false, "do not print column headings")
	// TODO needs alias here -- to `notruncate`
	flags.BoolVar(&opts.truncate, "no-trunc", false, "do not truncate output")
	flags.BoolVarP(&opts.quiet, "quiet", "q", false, "display only image IDs")

	rootCmd.AddCommand(imagesCommand)
}

func imagesCmd(c *cobra.Command, args []string, iopts *imageResults) error {

	name := ""
	if len(args) > 0 {
		if iopts.all {
			return errors.Errorf("when using the --all switch, you may not pass any images names or IDs")
		}

		if err := buildahcli.VerifyFlagsArgsOrder(args); err != nil {
			return err
		}
		if len(args) == 1 {
			name = args[0]
		} else {
			return errors.New("'buildah images' requires at most 1 argument")
		}
	}

	store, err := getStore(c)
	if err != nil {
		return err
	}

	systemContext, err := parse.SystemContextFromOptions(c)
	if err != nil {
		return errors.Wrapf(err, "error building system context")
	}

	images, err := store.Images()
	if err != nil {
		return errors.Wrapf(err, "error reading images")
	}

	if iopts.quiet && iopts.format != "" {
		return errors.Errorf("quiet and format are mutually exclusive")
	}

	opts := imageOptions{
		all:       iopts.all,
		digests:   iopts.digests,
		format:    iopts.format,
		json:      iopts.json,
		noHeading: iopts.noHeading,
		truncate:  !iopts.truncate,
		quiet:     iopts.quiet,
	}
	ctx := getContext()

	var params *filterParams
	if iopts.filter != "" {
		params, err = parseFilter(ctx, store, images, iopts.filter)
		if err != nil {
			return errors.Wrapf(err, "error parsing filter")
		}
	}

	return outputImages(ctx, systemContext, store, images, params, name, opts)
}

func parseFilter(ctx context.Context, store storage.Store, images []storage.Image, filter string) (*filterParams, error) {
	params := new(filterParams)
	filterStrings := strings.Split(filter, ",")
	for _, param := range filterStrings {
		pair := strings.SplitN(param, "=", 2)
		switch strings.TrimSpace(pair[0]) {
		case "dangling":
			if pair[1] == "true" || pair[1] == "false" {
				params.dangling = pair[1]
			} else {
				return nil, fmt.Errorf("invalid filter: '%s=[%s]'", pair[0], pair[1])
			}
		case "label":
			params.label = pair[1]
		case "before":
			beforeDate, err := setFilterDate(ctx, store, images, pair[1])
			if err != nil {
				return nil, fmt.Errorf("no such id: %s", pair[0])
			}
			params.beforeDate = beforeDate
			params.beforeImage = pair[1]
		case "since":
			sinceDate, err := setFilterDate(ctx, store, images, pair[1])
			if err != nil {
				return nil, fmt.Errorf("no such id: %s", pair[0])
			}
			params.sinceDate = sinceDate
			params.sinceImage = pair[1]
		case "reference":
			params.referencePattern = pair[1]
		case "readonly":
			if pair[1] == "true" || pair[1] == "false" {
				params.readOnly = pair[1]
			} else {
				return nil, fmt.Errorf("invalid filter: '%s=[%s]'", pair[0], pair[1])
			}
		default:
			return nil, fmt.Errorf("invalid filter: '%s'", pair[0])
		}
	}
	return params, nil
}

func setFilterDate(ctx context.Context, store storage.Store, images []storage.Image, imgName string) (time.Time, error) {
	for _, image := range images {
		for _, name := range image.Names {
			if matchesReference(name, imgName) {
				// Set the date to this image
				ref, err := is.Transport.ParseStoreReference(store, image.ID)
				if err != nil {
					return time.Time{}, fmt.Errorf("error parsing reference to image %q: %v", image.ID, err)
				}
				img, err := ref.NewImage(ctx, nil)
				if err != nil {
					return time.Time{}, fmt.Errorf("error reading image %q: %v", image.ID, err)
				}
				defer img.Close()
				inspect, err := img.Inspect(ctx)
				if err != nil {
					return time.Time{}, fmt.Errorf("error inspecting image %q: %v", image.ID, err)
				}
				date := *inspect.Created
				return date, nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("could not locate image %q", imgName)
}

func outputHeader(opts imageOptions) string {
	if opts.format != "" {
		return strings.Replace(opts.format, `\t`, "\t", -1)
	}
	if opts.quiet {
		return formats.IDString
	}
	format := "{{if .Name}}{{.Name}}{{else}}<none>{{end}}\t{{if .Tag}}{{.Tag}}{{else}}<none>{{end}}\t"
	if !opts.noHeading {
		format = "table " + format
	}

	if opts.digests {
		format += "{{.Digest}}\t"
	}
	format += "{{.ID}}\t{{.CreatedAt}}\t{{.Size}}"
	if opts.readOnly {
		format += "\t{{.ReadOnly}}"
	}
	return format
}

type imagesSorted []imageOutputParams

func outputImages(ctx context.Context, systemContext *types.SystemContext, store storage.Store, images []storage.Image, filters *filterParams, argName string, opts imageOptions) error {
	found := false
	var imagesParams imagesSorted
	jsonImages := []jsonImage{}

	for _, image := range images {
		if image.ReadOnly {
			opts.readOnly = true
		}
		createdTime := image.Created
		inspectedTime, size, _ := getDateAndSize(ctx, systemContext, store, image)
		if !inspectedTime.IsZero() {
			if createdTime != inspectedTime {
				logrus.Debugf("image record and configuration disagree on the image's creation time for %q, using the configuration creation time: %s", image.ID, inspectedTime)
				createdTime = inspectedTime
			}
		}
		createdTime = createdTime.Local()

		// If "all" is false and this image doesn't have a name, check
		// to see if the image is the parent of any other image.  If it
		// is, then it is an intermediate image, so don't list it if
		// the --all flag is not set.
		if !opts.all && len(image.Names) == 0 {
			isParent, err := imageIsParent(ctx, systemContext, store, &image)
			if err != nil {
				logrus.Errorf("error checking if image is a parent %q: %v", image.ID, err)
			}
			if isParent {
				continue
			}
		}

		imageID := "sha256:" + image.ID
		if opts.truncate {
			imageID = shortID(image.ID)
		}

		filterMatched := false

		var imageReposAndTags [][2]string
		var imageDigests []string
		for _, imageDigest := range image.Digests {
			imageDigests = append(imageDigests, imageDigest.String())
		}

		for _, name := range image.Names {
			if name == "" {
				logrus.Warnf("Found image with empty name")
				continue
			}
			named, err := reference.ParseNormalizedNamed(name)
			if err != nil {
				logrus.Warnf("Error parsing name %q: %v", name, err)
				continue
			}
			if name != named.String() {
				logrus.Debugf("Image name %q wasn't already in its normalized form (%q).", name, named.String())
			}

			if !matchesReference(name, argName) {
				continue
			}
			found = true

			if digested, ok := named.(reference.Digested); ok {
				digest := digested.Digest()
				digestPresent := false
				for _, imageDigest := range imageDigests {
					if imageDigest == digest.String() {
						digestPresent = true
					}
				}
				if !digestPresent {
					imageDigests = append(imageDigests)
				}
			}

			if !matchesFilter(ctx, store, image, name, filters) {
				continue
			}
			filterMatched = true

			if tagged, ok := named.(reference.Tagged); ok {
				imageReposAndTags = append(imageReposAndTags, [2]string{named.Name(), tagged.Tag()})
			} else {
				imageReposAndTags = append(imageReposAndTags, [2]string{named.Name(), ""})
			}
		}
		if len(image.Names) == 0 && matchesFilter(ctx, store, image, "", filters) {
			filterMatched = true
		}
		if !filterMatched {
			continue
		}

		if opts.json {
			jsonImages = append(jsonImages, jsonImage{
				ID:           image.ID,
				Names:        image.Names,
				Digest:       string(image.Digest),
				Digests:      imageDigests,
				CreatedAtRaw: createdTime,
				CreatedAt:    units.HumanDuration(time.Since((createdTime))) + " ago",
				Size:         formattedSize(size),
				ReadOnly:     image.ReadOnly,
			})
			continue
		}
		if len(imageReposAndTags) == 0 {
			imageReposAndTags = [][2]string{{"", ""}}
		}
		for _, imageRepoAndTag := range imageReposAndTags {
			imagesParams = append(imagesParams, imageOutputParams{
				ID:           imageID,
				Name:         imageRepoAndTag[0],
				Tag:          imageRepoAndTag[1],
				Digest:       string(image.Digest),
				Digests:      imageDigests,
				CreatedAtRaw: createdTime,
				CreatedAt:    units.HumanDuration(time.Since((createdTime))) + " ago",
				Size:         formattedSize(size),
				ReadOnly:     image.ReadOnly,
			})
			if opts.quiet {
				break
			}
		}
	}

	if !found && argName != "" {
		return errors.Errorf("No such image %s", argName)
	}
	if opts.json {
		data, err := json.MarshalIndent(jsonImages, "", "    ")
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", data)
		return nil
	}

	imagesParams = sortImagesOutput(imagesParams)
	out := formats.StdoutTemplateArray{Output: imagesToGeneric(imagesParams), Template: outputHeader(opts), Fields: imagesHeader}
	return formats.Writer(out).Out()
}

func shortID(id string) string {
	idTruncLength := 12
	if len(id) > idTruncLength {
		return id[:idTruncLength]
	}
	return id
}

func sortImagesOutput(imagesOutput imagesSorted) imagesSorted {
	sort.Sort(imagesOutput)
	return imagesOutput
}

func (a imagesSorted) Less(i, j int) bool {
	return a[i].CreatedAtRaw.After(a[j].CreatedAtRaw)
}
func (a imagesSorted) Len() int      { return len(a) }
func (a imagesSorted) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

func imagesToGeneric(templParams []imageOutputParams) (genericParams []interface{}) {
	if len(templParams) > 0 {
		for _, v := range templParams {
			genericParams = append(genericParams, interface{}(v))
		}
	}
	return genericParams
}

func matchesFilter(ctx context.Context, store storage.Store, image storage.Image, name string, params *filterParams) bool {
	if params == nil {
		return true
	}
	if params.dangling != "" && !matchesDangling(name, params.dangling) {
		return false
	}
	if params.label != "" && !matchesLabel(ctx, store, image, params.label) {
		return false
	}
	if params.beforeImage != "" && !matchesBeforeImage(image, params) {
		return false
	}
	if params.sinceImage != "" && !matchesSinceImage(image, params) {
		return false
	}
	if params.referencePattern != "" && !matchesReference(name, params.referencePattern) {
		return false
	}
	if params.readOnly != "" && !matchesReadOnly(image, params.readOnly) {
		return false
	}
	return true
}

func matchesDangling(name string, dangling string) bool {
	if dangling == "false" && name != "" {
		return true
	}
	if dangling == "true" && name == "" {
		return true
	}
	return false
}
func matchesReadOnly(image storage.Image, readOnly string) bool {
	if readOnly == "false" && !image.ReadOnly {
		return true
	}
	if readOnly == "true" && image.ReadOnly {
		return true
	}
	return false
}

func matchesLabel(ctx context.Context, store storage.Store, image storage.Image, label string) bool {
	storeRef, err := is.Transport.ParseStoreReference(store, image.ID)
	if err != nil {
		return false
	}
	img, err := storeRef.NewImage(ctx, nil)
	if err != nil {
		return false
	}
	defer img.Close()
	info, err := img.Inspect(ctx)
	if err != nil {
		return false
	}

	pair := strings.SplitN(label, "=", 2)
	for key, value := range info.Labels {
		if key == pair[0] {
			if len(pair) == 2 {
				if value == pair[1] {
					return true
				}
			} else {
				return false
			}
		}
	}
	return false
}

// Returns true if the image was created since the filter image.  Returns
// false otherwise
func matchesBeforeImage(image storage.Image, params *filterParams) bool {
	return image.Created.IsZero() || image.Created.Before(params.beforeDate)
}

// Returns true if the image was created since the filter image.  Returns
// false otherwise
func matchesSinceImage(image storage.Image, params *filterParams) bool {
	return image.Created.IsZero() || image.Created.After(params.sinceDate)
}

func matchesID(imageID, argID string) bool {
	return strings.HasPrefix(imageID, argID)
}

func matchesReference(imageName, argName string) bool {
	if argName == "" {
		return true
	}
	if imageName == "" {
		return false
	}
	named, err := reference.ParseNormalizedNamed(imageName)
	if err != nil {
		logrus.Warnf("Error parsing image name %q: %v", imageName, err)
		return false
	}
	// If the arg contains a tag, we handle it differently than if it does not: the tag must match exactly
	if strings.Contains(argName, ":") {
		splitArg := strings.Split(argName, ":")
		if tagged, ok := named.(reference.Tagged); ok {
			return (named.Name() == splitArg[0] || strings.HasSuffix(named.Name(), "/"+splitArg[0])) && (tagged.Tag() == splitArg[1])
		}
		return false
	}
	return named.Name() == argName || strings.HasSuffix(named.Name(), "/"+argName)
}

// According to  https://en.wikipedia.org/wiki/Binary_prefix
// We should be return numbers based on 1000, rather then 1024
func formattedSize(size int64) string {
	suffixes := [5]string{"B", "KB", "MB", "GB", "TB"}

	count := 0
	formattedSize := float64(size)
	for formattedSize >= 1000 && count < 4 {
		formattedSize /= 1000
		count++
	}
	return fmt.Sprintf("%.3g %s", formattedSize, suffixes[count])
}
