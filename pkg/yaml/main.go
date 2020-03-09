package yaml

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/mitchellh/mapstructure"
	"github.com/olblak/updateCli/pkg/git"
	"github.com/olblak/updateCli/pkg/github"
	"github.com/olblak/updateCli/pkg/scm"
	"gopkg.in/yaml.v3"
)

var (
	yamlIdent int = 2
)

// Yaml stores configuration about the file and the key value that needs to be updated
type Yaml struct {
	File       string
	Key        string
	Message    string
	Scm        string
	Repository interface{}
}

// searchAndUpdateVersion parses a yaml object looking for a specific key that need to be updated if needed
func searchAndUpdateVersion(entry *yaml.Node, keys []string, version string, columnRef int) (found bool, oldVersion string, column int) {

	valueFound := false
	column = columnRef
	nextLevel := false
	for _, content := range entry.Content {
		if content.Column < column {
			break
		}
		if nextLevel {
			column = content.Column
			nextLevel = false
		}

		if content.Value == keys[0] && (content.Column == columnRef) {
			column = content.Column
			nextLevel = true

			if len(keys) > 1 {
				keys = keys[1:]
			} else if len(keys) == 1 {
				valueFound = true
				continue
			}
		}

		if content.Kind == yaml.ScalarNode && valueFound == true {
			column = content.Column

			oldVersion = content.Value
			content.SetString(version)

			break
		} else if content.Kind == yaml.MappingNode {
			valueFound, oldVersion, column = searchAndUpdateVersion(content, keys, version, column)
		}
	}
	return valueFound, oldVersion, column
}

// Update updates a scm repository based on yaml modification
func (y *Yaml) Update(version string) {
	var scm scm.Scm

	switch y.Scm {
	case "git":
		var g git.Git

		err := mapstructure.Decode(y.Repository, &g)

		if err != nil {
			fmt.Println(err)
		}

		g.GetDirectory()

		scm = &g
	case "github":
		var g github.Github

		err := mapstructure.Decode(y.Repository, &g)

		if err != nil {
			fmt.Println(err)
		}

		g.GetDirectory()

		scm = &g
	default:
		fmt.Printf("Something went wrong while looking at yaml repository of kind\n")
	}

	fmt.Printf("\nUpdating key  '%s' from target file: %s:\n\n", y.Key, y.File)

	scm.Init(version)

	path := filepath.Join(scm.GetDirectory(), y.File)

	if _, err := os.Stat(path); os.IsNotExist(err) {
		scm.Clone()
	}

	file, err := os.Open(path)
	if err != nil {
		fmt.Println(err)
	}

	defer file.Close()

	data, err := ioutil.ReadAll(file)
	if err != nil {
		fmt.Println(err)
	}

	var out yaml.Node

	err = yaml.Unmarshal(data, &out)

	if err != nil {
		log.Printf("cannot unmarshal data: %v", err)
	}

	valueFound, oldVersion, _ := searchAndUpdateVersion(&out, strings.Split(y.Key, "."), version, 1)

	if valueFound {
		if oldVersion == version {
			fmt.Printf("\u2714 Value '%s' already up to date\n", version)
			return
		}

		fmt.Printf("\u2717 Version mismatched between %s (old) and %s (new)\n", oldVersion, version)

	} else {
		fmt.Printf("\u2717 cannot find key '%v' in file %v\n", y.Key, path)
		return
	}

	message := fmt.Sprintf("Updating key '%v' to %s",
		y.Key,
		version)

	fmt.Printf("%s\n", message)

	newFile, err := os.Create(path)
	defer newFile.Close()

	encoder := yaml.NewEncoder(newFile)
	defer encoder.Close()
	encoder.SetIndent(yamlIdent)
	err = encoder.Encode(&out)

	if err != nil {
		log.Fatalf("Something went wrong while encoding %v\n", err)
	}

	scm.Add(y.File)
	scm.Commit(y.File, message)
	scm.Push()
	scm.Clean()
}
