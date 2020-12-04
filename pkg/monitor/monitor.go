package monitor

import (
	"context"
	"fmt"
	"github.com/go-orm/gorm"
	_ "github.com/go-orm/gorm/dialects/sqlite"
	"github.com/lileio/pubsub/v2"
	"github.com/lileio/pubsub/v2/middleware/defaults"
	"github.com/niedbalski/go-athena/pkg/common"
	"github.com/niedbalski/go-athena/pkg/config"
	log "github.com/sirupsen/logrus"
	"regexp"
	"time"
)

type Monitor struct {
	Db               *gorm.DB
	Config           *config.Config
	FilesClient      common.FilesComClient
	SalesforceClient common.SalesforceClient
	Provider         pubsub.Provider
}

func (m *Monitor) GetMatchingProcessors(filename string, c *common.Case) ([]string, error) {
	var processors []string
	for _, processor := range m.Config.Monitor.ProcessorMap {
		switch processor.Type {
		case "filename":
			{
				if ok, _ := regexp.Match(processor.Regex, []byte(filename)); ok {
					processors = append(processors, processor.Processor)
				}
			}
		case "case":
			{
				if c == nil {
					continue
				}
				if ok, _ := regexp.Match(processor.Regex, []byte(c.CaseNumber)); ok {
					processors = append(processors, processor.Processor)
				}
			}
		default:
			fmt.Printf("Not found handler for %s type", processor.Type)
		}
	}
	if len(processors) <= 0 {
		return nil, fmt.Errorf("Not found processors for %s", filename)
	}
	return processors, nil
}

func (m *Monitor) GetLatestFiles(dirs []string, duration time.Duration) ([]common.File, error) {
	files, err := m.FilesClient.GetFiles(dirs)
	if err != nil {
		return nil, err
	}

	for _, file := range files {
		m.Db.Where(common.File{Path: file.Path, Md5sum: file.Md5sum, Created: time.Now()}).FirstOrCreate(&file)
	}

	now := time.Now()
	m.Db.Where("created >= ?", now.Add(-duration)).Find(&files)
	return files, nil
}

func (m *Monitor) GetMatchingProcessorByFile(files []common.File) (map[string][]common.File, error) {
	var sfCase = &common.Case{}
	var results = make(map[string][]common.File)

	for _, file := range files {
		var processors []string
		caseNumber, err := common.GetCaseNumberByFilename(file.Path)
		if err == nil && caseNumber != "" {
			sfCase, err = m.SalesforceClient.GetCaseByNumber(caseNumber)
			if err != nil {
				log.Error(err)
			}
		}

		if sfCase != nil {
			processors, err = m.GetMatchingProcessors(file.Path, sfCase)
		} else {
			processors, err = m.GetMatchingProcessors(file.Path, nil)
		}

		for _, processor := range processors {
			results[processor] = append(results[processor], file)
		}
		if err != nil {
			log.Error(err)
			continue
		}
	}

	return results, nil
}

func NewMonitor(filesClient common.FilesComClient, salesforceClient common.SalesforceClient, provider pubsub.Provider, cfg *config.Config, db *gorm.DB) (*Monitor, error) {
	if db == nil {
		var err error
		if db, err = gorm.Open("sqlite3", "main.db"); err != nil {
			return nil, err
		}
		db.AutoMigrate(common.File{})
	}
	return &Monitor{Provider: provider, Db: db, FilesClient: filesClient, SalesforceClient: salesforceClient, Config: cfg}, nil
}

func (m *Monitor) Run(ctx context.Context) error {
	pubsub.SetClient(&pubsub.Client{
		ServiceName: "athena-processor",
		Provider:    m.Provider,
		Middleware:  defaults.Middleware,
	})

	pollEvery, err := time.ParseDuration(m.Config.Monitor.PollEvery)
	if err != nil {
		return err
	}

	ticker := time.NewTicker(pollEvery)
	quit := make(chan struct{})

	go func() {
		for {
			select {
			case <-ticker.C:
				latestFiles, err := m.GetLatestFiles(m.Config.Monitor.Directories, common.DefaultFilesAgeDelta)
				if err != nil {
					log.Error(err)
					continue
				}

				processors, err := m.GetMatchingProcessorByFile(latestFiles)
				if err != nil {
					log.Error(err)
					continue
				}

				log.Infof("Found %d new files to be processed", len(latestFiles))

				for processor, files := range processors {
					for _, file := range files {
						log.Infof("Sending file: %s to processor: %s", file.Path, processor)
						if err := pubsub.PublishJSON(context.Background(), processor, file); err != nil {
							log.Error(err)
						}
					}
				}
			case <-quit:
				ticker.Stop()
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		{
			close(quit)
			return nil
		}
	}
}
