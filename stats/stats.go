/*
Copyright 2017 WALLIX

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package stats

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/wallix/awless/config"
	"github.com/wallix/awless/database"
	"github.com/wallix/awless/graph"
)

var (
	serverUrl          = "https://updates.awless.io"
	expirationDuration = 24 * time.Hour
)

func SendStats(db *database.DB, localInfra, localAccess *graph.Graph) error {
	publicKey, err := loadPublicKey()
	if err != nil {
		return err
	}
	lastCommandId, err := db.GetIntValue(database.SentIdKey)
	if err != nil {
		return err
	}

	s, lastCommandId, err := BuildStats(db, localInfra, localAccess, lastCommandId)
	if err != nil {
		return err
	}

	var zipped bytes.Buffer
	zippedW := gzip.NewWriter(&zipped)
	if err = json.NewEncoder(zippedW).Encode(s); err != nil {
		return err
	}
	zippedW.Close()

	sessionKey, encrypted, err := aesEncrypt(zipped.Bytes())
	if err != nil {
		return err
	}
	encryptedKey, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, publicKey, sessionKey, nil)
	if err != nil {
		return err
	}

	payload, err := json.Marshal(encryptedData{encryptedKey, encrypted})
	if err != nil {
		return err
	}

	err = sendPayloadAndCheckUpgrade(serverUrl, payload, os.Stderr)
	if err != nil {
		return err
	}

	if err := db.SetIntValue(database.SentIdKey, lastCommandId); err != nil {
		return err
	}
	if err := db.SetTimeValue(database.SentTimeKey, time.Now()); err != nil {
		return err
	}
	if err := db.DeleteLogs(); err != nil {
		return err
	}
	if err := db.DeleteHistory(); err != nil {
		return err
	}
	return nil
}

func sendPayloadAndCheckUpgrade(url string, payload []byte, w io.Writer) error {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	latest := struct {
		Version, URL string
	}{}

	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&latest); err == nil {
		if config.IsUpgrade(config.Version, latest.Version) {
			var install string
			switch config.BuildFor {
			case "brew":
				install = "Run `brew upgrade awless`"
			default:
				install = fmt.Sprintf("Run `wget -O awless-%s.zip https://github.com/wallix/awless/releases/download/%s/awless-%s-%s.zip`", latest.Version, latest.Version, runtime.GOOS, runtime.GOARCH)
			}
			fmt.Fprintf(w, "New version %s available. %s\n", latest.Version, install)
		}
	}

	return nil
}

func BuildStats(db *database.DB, infra *graph.Graph, access *graph.Graph, fromCommandId int) (*stats, int, error) {
	commandsStat, lastCommandId, err := buildCommandsStat(db, fromCommandId)
	if err != nil {
		return nil, 0, err
	}
	region := db.MustGetDefaultRegion()

	im := &infraMetrics{}
	if infra != nil {
		im, err = buildInfraMetrics(region, infra)
		if err != nil {
			return nil, 0, err
		}
	}

	am := &accessMetrics{}
	if access != nil {
		am, err = buildAccessMetrics(region, access, time.Now())
		if err != nil {
			return nil, 0, err
		}
	}

	is, err := buildInstancesStats(infra)
	if err != nil {
		return nil, 0, err
	}

	id, err := db.GetStringValue(database.AwlessIdKey)
	if err != nil {
		return nil, 0, err
	}

	aId, err := db.GetStringValue(database.AwlessAIdKey)
	if err != nil {
		return nil, 0, err
	}

	logs, err := db.GetLogs()
	if err != nil {
		return nil, 0, err
	}

	s := &stats{
		Id:             id,
		AId:            aId,
		Version:        config.Version,
		BuildInfo:      config.CurrentBuildInfo,
		Commands:       commandsStat,
		InfraMetrics:   im,
		InstancesStats: is,
		AccessMetrics:  am,
		Logs:           logs,
	}

	return s, lastCommandId, nil
}

func CheckStatsToSend(db *database.DB) bool {
	sent, err := db.GetTimeValue(database.SentTimeKey)
	if err != nil {
		sent = time.Time{}
	}
	return (time.Since(sent) > expirationDuration)
}

type stats struct {
	Id             string
	AId            string
	Version        string
	BuildInfo      config.BuildInfo
	Commands       []*dailyCommands
	InfraMetrics   *infraMetrics
	InstancesStats []*instancesStat
	AccessMetrics  *accessMetrics
	Logs           []*database.Log
}

type dailyCommands struct {
	Command string
	Hits    int
	Date    time.Time
}

type instancesStat struct {
	Type string
	Date time.Time
	Hits int
	Name string
}

type accessMetrics struct {
	Date                     time.Time
	Region                   string
	NbGroups                 int
	NbPolicies               int
	NbRoles                  int
	NbUsers                  int
	MinUsersByGroup          int
	MaxUsersByGroup          int
	MinUsersByLocalPolicies  int
	MaxUsersByLocalPolicies  int
	MinRolesByLocalPolicies  int
	MaxRolesByLocalPolicies  int
	MinGroupsByLocalPolicies int
	MaxGroupsByLocalPolicies int
}

func buildCommandsStat(db *database.DB, fromCommandId int) ([]*dailyCommands, int, error) {
	var commandsStat []*dailyCommands

	commandsHistory, err := db.GetHistory(fromCommandId)
	if err != nil {
		return commandsStat, 0, err
	}

	if len(commandsHistory) == 0 {
		return commandsStat, 0, nil
	}

	date := commandsHistory[0].Time
	commands := make(map[string]int)

	for _, command := range commandsHistory {
		if !sameDay(&date, &command.Time) {
			commandsStat = addDailyCommands(commandsStat, commands, &date)
			date = command.Time
			commands = make(map[string]int)
		}
		commands[strings.Join(command.Command, " ")] += 1
	}
	commandsStat = addDailyCommands(commandsStat, commands, &date)

	lastCommandId := commandsHistory[len(commandsHistory)-1].ID
	return commandsStat, lastCommandId, nil
}

func buildInstancesStats(infra *graph.Graph) (instancesStats []*instancesStat, err error) {
	instancesStats, err = addStatsForInstanceStringProperty(infra, "Type", "InstanceType", instancesStats)
	if err != nil {
		return instancesStats, err
	}
	instancesStats, err = addStatsForInstanceStringProperty(infra, "ImageId", "ImageId", instancesStats)
	if err != nil {
		return instancesStats, err
	}

	return instancesStats, err
}

func addStatsForInstanceStringProperty(infra *graph.Graph, propertyName string, instanceStatType string, instancesStats []*instancesStat) ([]*instancesStat, error) {
	instances, err := infra.GetAllResources(graph.Instance)
	if err != nil {
		return nil, err
	}

	propertyValuesCountMap := make(map[string]int)
	for _, inst := range instances {
		if inst.Properties[propertyName] != nil {
			propertyValue, ok := inst.Properties[propertyName].(string)
			if !ok {
				return nil, fmt.Errorf("Property value of '%s' is not a string: %T", inst.Properties[propertyName], inst.Properties[propertyName])
			}
			propertyValuesCountMap[propertyValue]++
		}
	}

	for k, v := range propertyValuesCountMap {
		instancesStats = append(instancesStats, &instancesStat{Type: instanceStatType, Date: time.Now(), Hits: v, Name: k})
	}

	return instancesStats, err
}

func addDailyCommands(commandsStat []*dailyCommands, commands map[string]int, date *time.Time) []*dailyCommands {
	for command, hits := range commands {
		dc := dailyCommands{Command: command, Hits: hits, Date: *date}
		commandsStat = append(commandsStat, &dc)
	}
	return commandsStat
}

type infraMetrics struct {
	Date                  time.Time
	Region                string
	NbVpcs                int
	NbSubnets             int
	MinSubnetsPerVpc      int
	MaxSubnetsPerVpc      int
	NbInstances           int
	MinInstancesPerSubnet int
	MaxInstancesPerSubnet int
}

func buildInfraMetrics(region string, infra *graph.Graph) (*infraMetrics, error) {
	metrics := &infraMetrics{
		Date:   time.Now(),
		Region: region,
	}

	c, min, max, err := computeCountMinMaxChildForType(infra, graph.Vpc)
	if err != nil {
		return metrics, err
	}
	metrics.NbVpcs, metrics.MinSubnetsPerVpc, metrics.MaxSubnetsPerVpc = c, min, max

	c, min, max, err = computeCountMinMaxChildForType(infra, graph.Subnet)
	if err != nil {
		return metrics, err
	}
	metrics.NbSubnets, metrics.MinInstancesPerSubnet, metrics.MaxInstancesPerSubnet = c, min, max

	c, _, _, err = computeCountMinMaxChildForType(infra, graph.Instance)
	if err != nil {
		return metrics, err
	}
	metrics.NbInstances = c

	return metrics, nil
}

func buildAccessMetrics(region string, access *graph.Graph, time time.Time) (*accessMetrics, error) {
	metrics := &accessMetrics{
		Date:   time,
		Region: region,
	}
	c, min, max, err := computeCountMinMaxForTypeWithChildType(access, graph.Group, graph.User)
	if err != nil {
		return metrics, err
	}
	metrics.NbGroups, metrics.MinUsersByGroup, metrics.MaxUsersByGroup = c, min, max

	c, min, max, err = computeCountMinMaxForTypeWithChildType(access, graph.Policy, graph.User)
	if err != nil {
		return metrics, err
	}
	metrics.NbPolicies, metrics.MinUsersByLocalPolicies, metrics.MaxUsersByLocalPolicies = c, min, max

	_, min, max, err = computeCountMinMaxForTypeWithChildType(access, graph.Policy, graph.Role)
	if err != nil {
		return metrics, err
	}
	metrics.MinRolesByLocalPolicies, metrics.MaxRolesByLocalPolicies = min, max

	_, min, max, err = computeCountMinMaxForTypeWithChildType(access, graph.Policy, graph.Group)
	if err != nil {
		return metrics, err
	}
	metrics.MinGroupsByLocalPolicies, metrics.MaxGroupsByLocalPolicies = min, max

	c, _, _, err = computeCountMinMaxChildForType(access, graph.Role)
	if err != nil {
		return metrics, err
	}
	metrics.NbRoles = c

	c, _, _, err = computeCountMinMaxChildForType(access, graph.User)
	if err != nil {
		return metrics, err
	}
	metrics.NbUsers = c

	return metrics, nil
}

func computeCountMinMaxChildForType(graph *graph.Graph, t graph.ResourceType) (int, int, int, error) {
	resources, err := graph.GetAllResources(t)
	if err != nil {
		return 0, 0, 0, err
	}
	if len(resources) == 0 {
		return 0, 0, 0, nil
	}
	firstRes := resources[0]
	count, err := graph.CountChildrenForNode(firstRes)
	if err != nil {
		return 0, 0, 0, err
	}

	min, max := count, count
	for _, res := range resources[1:] {
		count, err = graph.CountChildrenForNode(res)
		if err != nil {
			return 0, 0, 0, err
		}
		if count < min {
			min = count
		}
		if count > max {
			max = count
		}
	}
	return len(resources), min, max, nil
}

func computeCountMinMaxForTypeWithChildType(graph *graph.Graph, parentType, childType graph.ResourceType) (int, int, int, error) {
	resources, err := graph.GetAllResources(parentType)
	if err != nil {
		return 0, 0, 0, err
	}
	if len(resources) == 0 {
		return 0, 0, 0, nil
	}
	firstRes := resources[0]
	count, err := graph.CountChildrenOfTypeForNode(firstRes, childType)
	if err != nil {
		return 0, 0, 0, err
	}

	min, max := count, count
	for _, res := range resources[1:] {
		count, err = graph.CountChildrenOfTypeForNode(res, childType)
		if err != nil {
			return 0, 0, 0, err
		}
		if count < min {
			min = count
		}
		if count > max {
			max = count
		}
	}
	return len(resources), min, max, nil
}

func sameDay(date1, date2 *time.Time) bool {
	return (date1.Day() == date2.Day()) && (date1.Month() == date2.Month()) && (date1.Year() == date2.Year())
}

type encryptedData struct {
	Key  []byte
	Data []byte
}

func aesEncrypt(data []byte) ([]byte, []byte, error) {
	sessionKey := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, sessionKey); err != nil {
		return nil, nil, err
	}

	aesCipher, err := aes.NewCipher(sessionKey)
	if err != nil {
		return nil, nil, err
	}

	gcm, err := cipher.NewGCM(aesCipher)
	if err != nil {
		return nil, nil, err
	}

	nonce := make([]byte, gcm.NonceSize())

	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	encrypted := gcm.Seal(nonce, nonce, data, nil)
	return sessionKey, encrypted, nil
}
