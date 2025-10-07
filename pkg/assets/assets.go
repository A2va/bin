package assets

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/caarlos0/log"
	"github.com/cheggaaa/pb"
	"github.com/h2non/filetype"
	"github.com/h2non/filetype/matchers"
	"github.com/h2non/filetype/types"
	"github.com/krolaw/zipstream"
	"github.com/marcosnils/bin/pkg/config"
	"github.com/marcosnils/bin/pkg/options"
	bstrings "github.com/marcosnils/bin/pkg/strings"
	"github.com/xi2/xz"
)

var (
	msiType = filetype.AddType("msi", "application/octet-stream")
	ascType = filetype.AddType("asc", "text/plain")
)

type Asset struct {
	Name string
	// Some providers (like gitlab) have non-descriptive names for files,
	// so we're using this DisplayName as a helper to produce prettier
	// outputs for bin
	DisplayName string
	URL         string
}

func (g Asset) String() string {
	if g.DisplayName != "" {
		return g.DisplayName
	}
	return g.Name
}

type FilteredAsset struct {
	RepoName     string
	Name         string
	DisplayName  string
	URL          string
	score        int
	ExtraHeaders map[string]string
}

type finalFile struct {
	Source      io.Reader
	Name        string
	PackagePath string
}

type platformResolver interface {
	GetOS() []string
	GetArch() []string
	GetOSSpecificExtensions() []string
}

type Filter struct {
	opts        *FilterOpts
	repoName    string
	name        string
	packagePath string
}

type FilterOpts struct {
	SkipScoring   bool
	SkipPathCheck bool

	// In case of updates, we're sending the previous version package path
	// so in case it's the same one, we can re-use it.
	PackageName string

	// If target file is in a package format (tar, zip,etc) use this
	// variable to filter the resulting outputs. This is very useful
	// so we don't prompt the user to pick the file again on updates
	PackagePath string
}

type runtimeResolver struct{}

func (runtimeResolver) GetOS() []string {
	return config.GetOS()
}

func (runtimeResolver) GetArch() []string {
	return config.GetArch()
}

func (runtimeResolver) GetOSSpecificExtensions() []string {
	return config.GetOSSpecificExtensions()
}

var resolver platformResolver = runtimeResolver{}

func (g FilteredAsset) String() string {
	if g.DisplayName != "" {
		return g.DisplayName
	}
	return g.Name
}

func NewFilter(opts *FilterOpts) *Filter {
	return &Filter{opts: opts}
}

// FilterAssets receives a slice of GL assets and tries to
// select the proper one and ask the user to manually select one
// in case it can't determine it
func (f *Filter) FilterAssets(repoName string, as []*Asset) (*FilteredAsset, error) {
	matches := []*FilteredAsset{}
	if len(as) == 1 {
		a := as[0]
		matches = append(matches, &FilteredAsset{RepoName: repoName, Name: a.Name, URL: a.URL, score: 0})
	} else {
		if !f.opts.SkipScoring {
			scores := map[string]int{}
			scoreKeys := []string{}
			scores[repoName] = 1
			for _, os := range resolver.GetOS() {
				scores[os] = 10
			}
			for _, arch := range resolver.GetArch() {
				scores[arch] = 5
			}
			for _, osSpecificExtension := range resolver.GetOSSpecificExtensions() {
				scores[osSpecificExtension] = 15
			}

			for key := range scores {
				scoreKeys = append(scoreKeys, strings.ToLower(key))
			}

			for _, a := range as {
				highestScoreForAsset := 0
				gf := &FilteredAsset{RepoName: repoName, Name: a.Name, DisplayName: a.DisplayName, URL: a.URL, score: 0}
				candidate := a.Name
				candidateScore := 0
				if strings.Contains(strings.ToLower(candidate), "bin/") {
					candidateScore += 10
					log.Debugf("Candidate %s is in a bin folder. Adding score 20", candidate)
				}
				if bstrings.ContainsAny(strings.ToLower(candidate), scoreKeys) &&
					isSupportedExt(candidate) {
					for toMatch, score := range scores {
						if strings.Contains(strings.ToLower(candidate), strings.ToLower(toMatch)) {
							log.Debugf("Candidate %s contains %s. Adding score %d", candidate, toMatch, score)
							candidateScore += score
						}
					}
					if candidateScore > highestScoreForAsset {
						highestScoreForAsset = candidateScore
						gf.Name = candidate
						gf.score = candidateScore
					}
				}

				if highestScoreForAsset > 0 {
					matches = append(matches, gf)
				}
			}
			highestAssetScore := 0
			for i := range matches {
				if matches[i].score > highestAssetScore {
					highestAssetScore = matches[i].score
				}
			}
			for i := len(matches) - 1; i >= 0; i-- {
				if matches[i].score < highestAssetScore {
					log.Debugf("Removing %v (URL %v) with score %v lower than %v", matches[i].Name, matches[i].URL, matches[i].score, highestAssetScore)
					matches = append(matches[:i], matches[i+1:]...)
				} else {
					log.Debugf("Keeping %v (URL %v) with highest score %v", matches[i].Name, matches[i].URL, matches[i].score)
				}
			}

		} else {
			log.Debugf("--all flag was supplied, skipping scoring")
			for _, a := range as {
				matches = append(matches, &FilteredAsset{RepoName: repoName, Name: a.Name, DisplayName: a.DisplayName, URL: a.URL, score: 0})
			}
		}
	}

	var gf *FilteredAsset
	if len(matches) == 0 {
		return nil, fmt.Errorf("Could not find any compatible files")
	} else if len(matches) > 1 {
		generic := make([]fmt.Stringer, 0)
		for _, f := range matches {
			generic = append(generic, f)
		}

		sort.SliceStable(generic, func(i, j int) bool {
			return generic[i].String() < generic[j].String()
		})

		choice, err := options.Select("Multiple matches found, please select one:", generic)
		if err != nil {
			return nil, err
		}
		gf = choice.(*FilteredAsset)
		// TODO make user select the proper file
	} else {
		gf = matches[0]
	}

	return gf, nil
}

// SanitizeName removes irrelevant information from the
// file name in case it exists
func SanitizeName(name, version string) string {
	name = strings.ToLower(name)
	replacements := []string{}

	// TODO maybe instead of doing this put everything in a map (set) and then
	// generate the replacements? IDK.
	firstPass := true
	for _, osName := range resolver.GetOS() {
		for _, archName := range resolver.GetArch() {
			replacements = append(replacements, "_"+osName+archName, "")
			replacements = append(replacements, "-"+osName+archName, "")
			replacements = append(replacements, "."+osName+archName, "")

			if firstPass {
				replacements = append(replacements, "_"+archName, "")
				replacements = append(replacements, "-"+archName, "")
				replacements = append(replacements, "."+archName, "")
			}
		}

		replacements = append(replacements, "_"+osName, "")
		replacements = append(replacements, "-"+osName, "")
		replacements = append(replacements, "."+osName, "")

		firstPass = false

	}

	replacements = append(replacements, "_"+version, "")
	replacements = append(replacements, "_"+strings.TrimPrefix(version, "v"), "")
	replacements = append(replacements, "-"+version, "")
	replacements = append(replacements, "-"+strings.TrimPrefix(version, "v"), "")
	r := strings.NewReplacer(replacements...)
	return r.Replace(name)
}

// ProcessURL processes a FilteredAsset by uncompressing/unarchiving the URL of the asset.
func (f *Filter) ProcessURL(gf *FilteredAsset) ([]*finalFile, error) {
	f.name = gf.Name
	// We're not closing the body here since the caller is in charge of that
	req, err := http.NewRequest(http.MethodGet, gf.URL, nil)
	if err != nil {
		return nil, err
	}
	for name, value := range gf.ExtraHeaders {
		req.Header.Add(name, value)
	}
	log.Debugf("Checking binary from %s", gf.URL)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	if res.StatusCode > 299 || res.StatusCode < 200 {
		return nil, fmt.Errorf("%d response when checking binary from %s", res.StatusCode, gf.URL)
	}

	// We're caching the whole file into memory so we can prompt
	// the user which file they want to download

	log.Infof("Starting download of %s", gf.URL)
	bar := pb.Full.Start64(res.ContentLength)
	barReader := bar.NewProxyReader(res.Body)
	defer bar.Finish()
	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, barReader)
	if err != nil {
		return nil, err
	}
	bar.Finish()
	return f.processReader(buf)
}

func (f *Filter) processReader(r io.Reader) ([]*finalFile, error) {
	var buf bytes.Buffer
	tee := io.TeeReader(r, &buf)

	t, err := filetype.MatchReader(tee)
	if err != nil {
		return nil, err
	}

	outputFile := io.MultiReader(&buf, r)

	type processorFunc func(repoName string, r io.Reader) ([]*finalFile, error)
	var processor processorFunc
	switch t {
	case matchers.TypeGz:
		processor = f.processGz
	case matchers.TypeTar:
		processor = f.processTar
	case matchers.TypeXz:
		processor = f.processXz
	case matchers.TypeBz2:
		processor = f.processBz2
	case matchers.TypeZip:
		processor = f.processZip
	}

	if processor != nil {
		outFiles, err := processor(f.repoName, outputFile)
		if err != nil {
			return nil, err
		}

		if len(outFiles) == 1 {
			f.name = outFiles[0].Name
			f.packagePath = outFiles[0].PackagePath
			// In case of e.g. a .tar.gz, process the uncompressed archive by calling recursively
			return f.processReader(outFiles[0].Source)
		}
		return outFiles, nil
	}

	return []*finalFile{{Source: outputFile, Name: f.name, PackagePath: f.packagePath}}, err
}

// processGz receives a tar.gz file and returns the
// correct file for bin to download
func (f *Filter) processGz(name string, r io.Reader) ([]*finalFile, error) {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}

	return []*finalFile{{Source: gr, Name: gr.Name}}, nil
}

func (f *Filter) processTar(name string, r io.Reader) ([]*finalFile, error) {
	tr := tar.NewReader(r)
	var executables []*finalFile
	var otherRegularFiles []*Asset
	fileContents := make(map[string][]byte)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		} else if header.FileInfo().IsDir() || header.Typeflag != tar.TypeReg {
			continue
		}

		if !f.opts.SkipPathCheck && len(f.opts.PackagePath) > 0 && header.Name != f.opts.PackagePath {
			continue
		}

		bs, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		fileContents[header.Name] = bs

		kind, _ := filetype.Match(bs)
		if isExecutable(kind) {
			log.Debugf("Found executable in tar: %s", header.Name)
			executables = append(executables, &finalFile{
				Source:      bytes.NewReader(bs),
				Name:        filepath.Base(header.Name),
				PackagePath: header.Name,
			})
		} else if !isArchive(kind) {
			otherRegularFiles = append(otherRegularFiles, &Asset{Name: header.Name})
		}
	}

	if len(executables) > 0 {
		log.Debugf("Found %d executables, returning all of them.", len(executables))
		return executables, nil
	}

	log.Debug("No executables found based on mimetype, falling back to asset filtering.")
	if len(otherRegularFiles) == 0 {
		return nil, fmt.Errorf("no files found in tar archive, use -p flag to manually select . PackagePath [%s]", f.opts.PackagePath)
	}

	choice, err := f.FilterAssets(name, otherRegularFiles)
	if err != nil {
		return nil, err
	}
	selectedFile := choice.String()

	tf := fileContents[selectedFile]

	return []*finalFile{{Source: bytes.NewReader(tf), Name: filepath.Base(selectedFile), PackagePath: selectedFile}}, nil
}

func (f *Filter) processBz2(name string, r io.Reader) ([]*finalFile, error) {
	br := bzip2.NewReader(r)

	return []*finalFile{{Source: br, Name: name}}, nil
}

func (f *Filter) processXz(name string, r io.Reader) ([]*finalFile, error) {
	xr, err := xz.NewReader(r, 0)
	if err != nil {
		return nil, err
	}

	return []*finalFile{{Source: xr, Name: name}}, nil
}

func (f *Filter) processZip(name string, r io.Reader) ([]*finalFile, error) {
	zr := zipstream.NewReader(r)
	var executables []*finalFile
	var otherRegularFiles []*Asset
	fileContents := make(map[string][]byte)

	for {
		header, err := zr.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		} else if header.Mode().IsDir() {
			continue
		}

		if !f.opts.SkipPathCheck && len(f.opts.PackagePath) > 0 && header.Name != f.opts.PackagePath {
			continue
		}

		bs, err := io.ReadAll(zr)
		if err != nil {
			return nil, err
		}
		fileContents[header.Name] = bs

		kind, _ := filetype.Match(bs)
		if isExecutable(kind) {
			log.Debugf("Found executable in zip: %s", header.Name)
			executables = append(executables, &finalFile{
				Source:      bytes.NewReader(bs),
				Name:        filepath.Base(header.Name),
				PackagePath: header.Name,
			})
		} else if !isArchive(kind) {
			otherRegularFiles = append(otherRegularFiles, &Asset{Name: header.Name})
		}
	}

	if len(executables) > 0 {
		log.Debugf("Found %d executables, returning all of them.", len(executables))
		return executables, nil
	}

	log.Debug("No executables found based on mimetype, falling back to asset filtering.")
	if len(otherRegularFiles) == 0 {
		return nil, fmt.Errorf("No files found in zip archive. PackagePath [%s]", f.opts.PackagePath)
	}

	choice, err := f.FilterAssets(name, otherRegularFiles)
	if err != nil {
		return nil, err
	}
	selectedFile := choice.String()

	fr := bytes.NewReader(fileContents[selectedFile])

	return []*finalFile{{Name: filepath.Base(selectedFile), Source: fr, PackagePath: selectedFile}}, nil
}

func isExecutable(kind types.Type) bool {
	return kind.MIME.Value == "application/vnd.microsoft.portable-executable" ||
		kind.MIME.Value == "application/x-elf" ||
		kind.MIME.Value == "application/x-mach-binary"
}

func isArchive(kind types.Type) bool {
	return kind == matchers.TypeGz ||
		kind == matchers.TypeTar ||
		kind == matchers.TypeXz ||
		kind == matchers.TypeBz2 ||
		kind == matchers.TypeZip
}

// isSupportedExt checks if this provider supports
// dealing with this specific file extension
func isSupportedExt(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".sha256", ".sha512", ".md5", ".sum", ".sig", ".asc":
		log.Debugf("Filename %s is a checksum/signature file, skipping", filename)
		return false
	}

	if ext := strings.TrimPrefix(filepath.Ext(filename), "."); len(ext) > 0 {
		switch filetype.GetType(ext) {
		case msiType, matchers.TypeDeb, matchers.TypeRpm:
			log.Debugf("Filename %s doesn't have a supported extension", filename)
			return false
		case matchers.TypeGz, types.Unknown, matchers.TypeZip, matchers.TypeXz, matchers.TypeTar, matchers.TypeBz2, matchers.TypeExe:
			break
		default:
			log.Debugf("Filename %s doesn't have a supported extension", filename)
			return false
		}
	}

	return true
}
