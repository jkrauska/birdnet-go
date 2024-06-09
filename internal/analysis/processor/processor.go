// processor.go
package processor

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/tphakala/birdnet-go/internal/analysis/queue"
	"github.com/tphakala/birdnet-go/internal/birdnet"
	"github.com/tphakala/birdnet-go/internal/birdweather"
	"github.com/tphakala/birdnet-go/internal/conf"
	"github.com/tphakala/birdnet-go/internal/datastore"
	"github.com/tphakala/birdnet-go/internal/mqtt"
	"github.com/tphakala/birdnet-go/internal/observation"
	"github.com/tphakala/birdnet-go/internal/telemetry"
)

type Processor struct {
	Settings           *conf.Settings
	Ds                 datastore.Interface
	Bn                 *birdnet.BirdNET
	BwClient           *birdweather.BwClient
	MqttClient         *mqtt.Client
	EventTracker       *EventTracker
	DogBarkFilter      DogBarkFilter
	SpeciesConfig      SpeciesConfig
	IncludedSpecies    *[]string
	SpeciesListUpdated time.Time
	//AudioBuffer        *myaudio.AudioBuffer
	//AudioBuffers       map[string]*myaudio.AudioBuffer
	LastDogDetection   map[string]time.Time // keep track of dog barks per audio source
	LastHumanDetection map[string]time.Time // keep track of human vocal per audio source
	Metrics            *telemetry.Metrics
}

type Detections struct {
	pcmData3s []byte              // 3s PCM data containing the detection
	Note      datastore.Note      // Note containing highest match
	Results   []datastore.Results // Full BirdNET prediction results
}

// PendingDetection struct represents a single detection held in memory,
// including its last updated timestamp and a deadline for flushing it to the worker queue.
type PendingDetection struct {
	Detection     Detections // The detection data
	Confidence    float64    // Confidence level of the detection
	Source        string     // Audio source of the detection, RTSP URL or audio card name
	HumanDetected bool       // Flag to indicate if the clip contains human vocal
	FirstDetected time.Time  // Time the detection was first detected
	LastUpdated   time.Time  // Last time this detection was updated
	FlushDeadline time.Time  // Deadline by which the detection must be processed
}

// PendingDetections is a map used to store detections temporarily.
// The map's keys are species' common names, and its values are PendingDetection structs.
var PendingDetections map[string]PendingDetection = make(map[string]PendingDetection)

// mutex is used to synchronize access to the PendingDetections map,
// ensuring thread safety when the map is accessed or modified by concurrent goroutines.
var mutex sync.Mutex

// func New(settings *conf.Settings, ds datastore.Interface, bn *birdnet.BirdNET, audioBuffers map[string]*myaudio.AudioBuffer, metrics *telemetry.Metrics) *Processor {
func New(settings *conf.Settings, ds datastore.Interface, bn *birdnet.BirdNET, metrics *telemetry.Metrics) *Processor {
	p := &Processor{
		Settings:           settings,          // BirdNET-Go Settings struct
		Ds:                 ds,                // Datastore
		Bn:                 bn,                // BirdNET analyzer
		EventTracker:       NewEventTracker(), // Duplicate event tracker
		IncludedSpecies:    new([]string),     // Included species list
		Metrics:            metrics,           // Prometheus metrics struct
		LastDogDetection:   make(map[string]time.Time),
		LastHumanDetection: make(map[string]time.Time),
	}

	// Start the detection processor
	p.startDetectionProcessor()

	// Start the worker pool for action processing
	p.startWorkerPool(10)

	// Start the held detection flusher
	p.pendingDetectionsFlusher()

	// Load Species configs
	p.SpeciesConfig, _ = LoadSpeciesConfig(conf.SpeciesConfigCSV)

	// Load dog bark filter config
	p.DogBarkFilter, _ = LoadDogBarkFilterConfig(conf.DogBarkFilterCSV)

	// Initialize BirdWeather client if enabled in settings.
	if settings.Realtime.Birdweather.Enabled {
		p.BwClient = birdweather.New(settings)
	}

	// Initialize MQTT client if enabled in settings.
	if settings.Realtime.MQTT.Enabled {
		p.MqttClient = mqtt.New(settings)

		// Connect to the MQTT broker in a separate goroutine to avoid blocking the main thread.
		go func() {
			log.Println("Connecting to MQTT broker")
			err := p.MqttClient.Connect()
			if err != nil {
				log.Printf("Failed to connect to MQTT broker: %s", err)
			} else {
				log.Println("Successfully connected to MQTT broker")
			}
		}()
	}

	var err error

	// Initialize included species list
	today := time.Now().Truncate(24 * time.Hour)

	// Update location based species list
	speciesScores, err := bn.GetProbableSpecies(today, 0.0)
	if err != nil {
		log.Printf("Failed to get probable species: %s", err)
	}

	// Convert the speciesScores slice to a slice of species labels
	var includedSpecies []string
	for _, speciesScore := range speciesScores {
		includedSpecies = append(includedSpecies, speciesScore.Label)
	}

	*p.IncludedSpecies = includedSpecies
	p.SpeciesListUpdated = today

	return p
}

// Start goroutine to process detections from the queue
func (p *Processor) startDetectionProcessor() {
	go func() {
		// ResultsQueue is fed by myaudio.ProcessData()
		for item := range queue.ResultsQueue {
			p.processDetections(item)
		}
	}()
}

// processDetections examines each detection from the queue, updating held detections
// with new or higher-confidence instances and setting an appropriate flush deadline.
func (p *Processor) processDetections(item *queue.Results) {
	const delay = 12 * time.Second // Delay before a detection is considered final and is flushed.

	for _, detection := range p.processResults(item) {
		commonName := strings.ToLower(detection.Note.CommonName)
		confidence := detection.Note.Confidence

		mutex.Lock() // Lock the mutex to ensure thread safety when accessing the PendingDetections map.
		// Check if this is a new detection or one with higher confidence.
		if pendingDetection, exists := PendingDetections[commonName]; !exists || confidence > pendingDetection.Confidence {
			if !exists {
				log.Printf("New detection: %s with confidence: %f, source: %v\n", detection.Note.CommonName, confidence, item.Source)
			}

			// Set the flush deadline for new detections or if the current detection's deadline has passed.
			firstDetected := item.StartTime // Set the begin time to the time it was first detected
			flushDeadline := firstDetected.Add(delay)

			if exists {
				log.Printf("Updating detection: %s with confidence: %f, source: %v\n", detection.Note.CommonName, confidence, item.Source)
				// If updating an existing detection, keep the original firstDetected timestamp.
				firstDetected = pendingDetection.FirstDetected
				if !time.Now().After(pendingDetection.FlushDeadline) {
					// If within the original deadline, do not update the flush deadline.
					flushDeadline = pendingDetection.FlushDeadline
				}
			}

			// Update the detection in the map.
			PendingDetections[commonName] = PendingDetection{
				Detection:     detection,
				Confidence:    confidence,
				FirstDetected: firstDetected,
				FlushDeadline: flushDeadline,
				LastUpdated:   time.Now(),
			}
		}
		mutex.Unlock() // Unlock the mutex after updating the map
	}
}

// processResults processes the results from the BirdNET prediction and returns a list of detections.
func (p *Processor) processResults(item *queue.Results) []Detections {
	var detections []Detections

	// item.Results could contain up to 10 results, process all of them
	for _, result := range item.Results {
		scientificName, commonName, _ := observation.ParseSpeciesString(result.Species)

		// Convert species to lowercase for case-insensitive comparison
		speciesLowercase := strings.ToLower(commonName)

		// Dog detection handling
		if p.Settings.Realtime.DogBarkFilter.Enabled {
			if strings.Contains(speciesLowercase, "dog") && result.Confidence > p.Settings.Realtime.DogBarkFilter.Confidence {
				log.Printf("Dog detected, updating last detection timestamp for potential owl false positives")
				p.LastDogDetection[item.Source] = time.Now()
			}
		}

		// Human detection handling for privacy filter
		if p.Settings.Realtime.PrivacyFilter.Enabled {
			if strings.Contains(speciesLowercase, "human") && result.Confidence > p.Settings.Realtime.PrivacyFilter.Confidence {
				log.Printf("Human detected, confidence %.6f", result.Confidence)
				// now minus 4 seconds
				p.LastHumanDetection[item.Source] = time.Now().Add(-4 * time.Second)
			}
		}

		// Use custom confidence threshold if it exists for the species, otherwise use the global threshold
		confidenceThreshold, exists := p.SpeciesConfig.Threshold[speciesLowercase]
		if !exists {
			confidenceThreshold = float32(p.Settings.BirdNET.Threshold)
		} else {
			if p.Settings.Debug {
				log.Printf("\nUsing confidence threshold of %.2f for %s\n", confidenceThreshold, speciesLowercase)
			}
		}

		if result.Confidence <= confidenceThreshold {
			// confidence too low, skip processing
			continue
		}

		// match against location based filter
		if !isSpeciesIncluded(result.Species, *p.IncludedSpecies) {
			if p.Settings.Debug {
				log.Printf("Species not on included list: %s\n", commonName)
			}
			continue
		}

		// create file name for audio clip
		item.ClipName = p.generateClipName(scientificName, result.Confidence)

		beginTime, endTime := 0.0, 0.0
		note := observation.New(p.Settings, beginTime, endTime, result.Species, float64(result.Confidence), item.Source, item.ClipName, item.ElapsedTime)

		// detection passed all filters, process it
		detections = append(detections, Detections{
			pcmData3s: item.PCMdata,
			Note:      note,
			Results:   item.Results,
		})
	}

	return detections
}

func (p *Processor) generateClipName(scientificName string, confidence float32) string {
	// Get the base path from the configuration
	basePath := conf.GetBasePath(p.Settings.Realtime.Audio.Export.Path)

	// Replace whitespaces with underscores and convert to lowercase
	formattedName := strings.ToLower(strings.ReplaceAll(scientificName, " ", "_"))

	// Normalize the confidence value to a percentage and append 'p'
	normalizedConfidence := confidence * 100
	formattedConfidence := fmt.Sprintf("%.0fp", normalizedConfidence)

	// Get the current time
	currentTime := time.Now()

	// Format the timestamp in ISO 8601 format
	timestamp := currentTime.Format("20060102T150405Z")

	// Extract the year and month for directory structure
	year := currentTime.Format("2006")
	month := currentTime.Format("01")

	// Set the file extension
	fileType := "wav"

	// Construct the clip name with the new pattern, including year and month subdirectories
	clipName := fmt.Sprintf("%s/%s/%s/%s_%s_%s.%s", basePath, year, month, formattedName, formattedConfidence, timestamp, fileType)

	return clipName
}

// isSpeciesIncluded checks if the given species is in the included species list.
// It returns true if the species is in the list, or if the list is empty (no filtering).
func isSpeciesIncluded(species string, includedList []string) bool {
	if len(includedList) == 0 {
		return true // no filtering applied when the list is empty
	}
	for _, s := range includedList {
		if species == s {
			return true
		}
	}

	return false
}

// pendingDetectionsFlusher runs a goroutine that periodically checks the pending detections
// and flushes them to the worker queue if their deadline has passed.
func (p *Processor) pendingDetectionsFlusher() {
	go func() {
		// Create a ticker that ticks every second to frequently check for flush deadlines.
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop() // Ensure the ticker is stopped to avoid leaking resources.

		for {
			<-ticker.C // Wait for the ticker to tick.
			now := time.Now()

			mutex.Lock() // Lock the mutex to ensure thread safety when accessing the PendingDetections map.
			for species, item := range PendingDetections {
				// If the current time is past the flush deadline, process the detection.
				if now.After(item.FlushDeadline) {
					// Check if human was detected after the first detection and discard if so
					if p.Settings.Realtime.PrivacyFilter.Enabled {
						if !strings.Contains(item.Detection.Note.CommonName, "Human") &&
							p.LastHumanDetection[item.Source].After(item.FirstDetected) {
							log.Printf("Discarding detection of %s from source %s due to privacy filter\n", species, item.Source)
							delete(PendingDetections, species)
							continue
						}
					}

					// Check dog bark filter
					if p.Settings.Realtime.DogBarkFilter.Enabled {
						if p.Settings.Realtime.DogBarkFilter.Debug {
							log.Printf("Last dog detection: %s\n", p.LastDogDetection)
						}
						// Check against common name
						if p.DogBarkFilter.Check(item.Detection.Note.CommonName, p.LastDogDetection[item.Source]) {
							log.Printf("Discarding detection of %s from source %s due to recent dog bark\n", item.Detection.Note.CommonName, item.Source)
							delete(PendingDetections, species)
							continue
						}
						// Check against scientific name
						if p.DogBarkFilter.Check(item.Detection.Note.ScientificName, p.LastDogDetection[item.Source]) {
							log.Printf("Discarding detection of %s from source %s due to recent dog bark\n", item.Detection.Note.CommonName, item.Source)
							delete(PendingDetections, species)
							continue
						}
					}

					// Set the detection's begin time to the time it was first detected, this is
					// where we start audio export for the detection.
					item.Detection.Note.BeginTime = item.FirstDetected
					// Retrieve and execute actions based on the held detection.
					actionList := p.getActionsForItem(item.Detection)
					for _, action := range actionList {
						workerQueue <- Task{Type: TaskTypeAction, Detection: item.Detection, Action: action}
					}
					// Detection is now processed, remove it from pending detections map.
					delete(PendingDetections, species)

					// Update Prometheus metrics detection counter
					if p.Settings.Realtime.Telemetry.Enabled {
						p.Metrics.IncrementDetectionCounter(item.Detection.Note.CommonName)
					}
				}
			}
			mutex.Unlock() // Unlock the mutex after updating the map.
		}
	}()
}
