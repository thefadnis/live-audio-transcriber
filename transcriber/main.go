package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	speech "cloud.google.com/go/speech/apiv1p1beta1"
	redis "github.com/go-redis/redis/v7"
	speechpb "google.golang.org/genproto/googleapis/cloud/speech/v1p1beta1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
)

var (
	// electionPort = flag.Int("electionPort", 4040,"leader election")
	redisHost          = flag.String("redisHost", "localhost", "Redis IP")
	audioQueue         = flag.String("audioQueue", "liveq", "input audio data")
	transcriptionQueue = flag.String("transcriptionQueue", "transcriptions", "output transcriptions")
	recoveryQueue      = flag.String("recoveryQueue", "recoverq", "recent audio data")
	recoveryRetain     = flag.Duration("recoveryRetainLast", 3*time.Second, "Retain  duration ")
	recoveryExpiry     = flag.Duration("recoveryExpiry", 30*time.Second, "Expire data ")
	flushTimeout       = flag.Duration("flushTimeout", 3000*time.Millisecond, "pending transcriptions emitted after time")
	pendingWordCount   = flag.Int("pendingWordCount", 4, "Treat last N are pending")
	sampleRate         = flag.Int("sampleRate", 16000, "(Hz)")
	channels           = flag.Int("channels", 1, "# audio channels")
	lang               = flag.String("lang", "en-US", "language code")
	phrases            = flag.String("phrases", "", " hints for Speech API")

	redisClient        *redis.Client
	recoveryRetainSize = int64(*recoveryRetain / (100 * time.Millisecond)) // each audio element ~100ms
	latestTranscript   string
	lastIndex          = 0
	pending            []string
)

func main() {
	flag.Parse()
	klog.InitFlags(nil)

	redisClient = redis.NewClient(&redis.Options{
		Addr:        *redisHost + ":6379",
		Password:    "", // no password set
		DB:          0,  // use default DB
		DialTimeout: 3 * time.Second,
		ReadTimeout: 4 * time.Second,
	})

	speechClient, err := speech.NewClient(context.Background())
	if err != nil {
		klog.Fatal(err)
	}

	contextPhrases := []string{}
	if *phrases != "" {
		contextPhrases = strings.Split(*phrases, ",")
		klog.Infof("Supplying %d phrase hints: %+q", len(contextPhrases), contextPhrases)
	}
	streamingConfig := speechpb.StreamingRecognitionConfig{
		Config: &speechpb.RecognitionConfig{
			Encoding:                   speechpb.RecognitionConfig_LINEAR16,
			SampleRateHertz:            int32(*sampleRate),
			AudioChannelCount:          int32(*channels),
			LanguageCode:               *lang,
			EnableAutomaticPunctuation: true,
			SpeechContexts: []*speechpb.SpeechContext{
				{Phrases: contextPhrases},
			},
		},
		InterimResults: true,
	}

	// if *electionPort > 0 {
	// 	var ctx context.Context
	// 	var cancel context.CancelFunc
	// 	addr := fmt.Sprintf(":%d", *electionPort)
	// 	server := &http.Server{Addr: addr}

	// 	ch := make(chan os.Signal, 1)
	// 	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	// 	go func() {
	// 		<-ch
	// 		if cancel != nil {
	// 			cancel()
	// 		}
	// 		klog.Info("Received termination, stopping transciptions")
	// 		if err := server.Shutdown(context.TODO()); err != nil {
	// 			klog.Fatalf("Error shutting down HTTP server: %v", err)
	// 		}
	// 	}()

	// 	isLeader := false
	// 	webHandler := func(res http.ResponseWriter, req *http.Request) {

	// 		if strings.Contains(req.URL.Path, "start") {
	// 			if !isLeader {
	// 				isLeader = true
	// 				klog.Infof("I became the leader! Starting goroutine to send audio")
	// 				ctx, cancel = context.WithCancel(context.Background())
	// 				go sendAudio(ctx, speechClient, streamingConfig)
	// 			}
	// 		}
	// 		if strings.Contains(req.URL.Path, "stop") {
	// 			if isLeader {
	// 				isLeader = false
	// 				klog.Infof("I stopped being the leader!")
	// 				cancel()
	// 			}
	// 		}
	// 		res.WriteHeader(http.StatusOK)
	// 	}
	// 	http.HandleFunc("/", webHandler)
	// 	klog.Infof("Starting leader election listener at port %s", addr)
	// 	server.ListenAndServe()
	// } else {
	// 	klog.Info("Not doing leader election")
	// 	sendAudio(context.Background(), speechClient, streamingConfig)
	// }
	sendAudio(context.Background(), speechClient, streamingConfig)
}

// Consumes queued audio data from Redis, and sends to Speech.
func sendAudio(ctx context.Context, speechClient *speech.Client, config speechpb.StreamingRecognitionConfig) {
	var stream speechpb.Speech_StreamingRecognizeClient
	receiveChan := make(chan bool)
	doRecovery := redisClient.Exists(*recoveryQueue).Val() > 0
	replaying := false

	for {
		select {
		case <-ctx.Done():
			klog.Infof("Context cancelled, exiting sender loop")
			return
		case _, ok := <-receiveChan:
			if !ok && stream != nil {
				klog.Info("receive channel closed, resetting stream")
				stream = nil
				receiveChan = make(chan bool)
				resetIndex()
				continue
			}
		default:
			// Process audio
		}

		var result string
		var err error
		if doRecovery {
			result, err = redisClient.RPop(*recoveryQueue).Result()
			if err == redis.Nil { // no more recovery data
				doRecovery = false
				replaying = false
				continue
			}
			if err == nil && !replaying {
				replaying = true
				emit("[REPLAY]")
			}
		} else {
			result, err = redisClient.BRPopLPush(*audioQueue, *recoveryQueue, 5*time.Second).Result()
			if err == redis.Nil { // pop timeout
				continue
			}
			// Temporarily retain the last few seconds of recent audio
			if err == nil {
				redisClient.LTrim(*recoveryQueue, 0, recoveryRetainSize)
				redisClient.Expire(*recoveryQueue, *recoveryExpiry)
			}
		}
		if err != nil && err != redis.Nil {
			klog.Errorf("Could not read from Redis: %v", err)
			doRecovery = true
			continue
		}

		// start a go routine to listen for responses
		if stream == nil {
			stream = initStreamingRequest(ctx, speechClient, config)
			go receiveResponses(stream, receiveChan)
			emit("[NEW STREAM]")
		}

		// Send audio, transcription responses received asynchronously
		decoded, _ := base64.StdEncoding.DecodeString(result)
		sendErr := stream.Send(&speechpb.StreamingRecognizeRequest{
			StreamingRequest: &speechpb.StreamingRecognizeRequest_AudioContent{
				AudioContent: decoded,
			},
		})
		if sendErr != nil {
			// Expected - if stream has been closed (e.g. timeout)
			if sendErr == io.EOF {
				continue
			}
			klog.Errorf("Could not send audio: %v", sendErr)
		}
	}
}

// Consumes StreamingResponses from Speech.
func receiveResponses(stream speechpb.Speech_StreamingRecognizeClient, receiveChan chan bool) {
	defer close(receiveChan)

	timer := time.NewTimer(*flushTimeout)
	go func() {
		<-timer.C
		flush()
	}()
	defer timer.Stop()

	// Consume streaming responses from Speech
	for {
		resp, err := stream.Recv()
		if err != nil {
			if status.Code(err) == codes.Canceled {
				return
			}
			klog.Errorf("Cannot stream results: %v", err)
			return
		}
		if err := resp.Error; err != nil {
			if status.FromProto(err).Code() == codes.OutOfRange {
				klog.Info("Timeout from API; closing connection")
				return
			}
			klog.Errorf("Could not recognize: %v", err)
			return
		}
		// If nothing received for a time, stop receiving
		if !timer.Stop() {
			return
		}
		timer.Reset(*flushTimeout)
		processResponses(*resp)
	}
}

// Handles transcription results.
func processResponses(resp speechpb.StreamingRecognizeResponse) {
	if len(resp.Results) == 0 {
		return
	}
	result := resp.Results[0]
	alternative := result.Alternatives[0]
	latestTranscript = alternative.Transcript
	elements := strings.Split(alternative.Transcript, " ")
	length := len(elements)

	if result.GetIsFinal() || alternative.GetConfidence() > 0 {
		klog.Info("Final result! Resetting")
		final := elements[lastIndex:]
		emit(strings.Join(final, " "))
		resetIndex()
		return
	}
	if result.Stability < 0.75 {
		klog.Infof("Ignoring low stability result (%v): %s", resp.Results[0].Stability,
			resp.Results[0].Alternatives[0].Transcript)
		return
	}

	unstable := ""
	if len(resp.Results) > 1 {
		unstable = resp.Results[1].Alternatives[0].Transcript
	}
	// Treat last N words as pending.
	if length < *pendingWordCount {
		lastIndex = 0
		pending = elements
		emitStages([]string{}, pending, unstable)
	} else if lastIndex < length-*pendingWordCount {
		steady := elements[lastIndex:(length - *pendingWordCount)]
		lastIndex += len(steady)
		pending = elements[lastIndex:]
		emitStages(steady, pending, unstable)
	}
}

func initStreamingRequest(ctx context.Context, client *speech.Client, config speechpb.StreamingRecognitionConfig) speechpb.Speech_StreamingRecognizeClient {
	stream, err := client.StreamingRecognize(ctx)
	if err != nil {
		klog.Fatal(err)
	}
	if err := stream.Send(&speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: &config,
		},
	}); err != nil {
		klog.Errorf("Error sending initial config message: %v", err)
		return nil
	}
	klog.Info("Initialised new connection to Speech API")
	return stream
}

func emitStages(steady []string, pending []string, unstable string) {
	msg := fmt.Sprintf("%s|%s|%s", strings.Join(steady, " "),
		strings.Join(pending, " "), unstable)
	emit(msg)
}

func emit(msg string) {
	klog.Info(msg)
	redisClient.LPush(*transcriptionQueue, msg)
}

func flush() {
	msg := ""
	if pending != nil {
		msg += strings.Join(pending, " ")
	}
	if msg != "" {
		klog.Info("Flushing...")
		emit("[FLUSH] " + msg)
	}
	resetIndex()
}

func resetIndex() {
	lastIndex = 0
	pending = nil
}

// Debugging
func logResponses(resp speechpb.StreamingRecognizeResponse) {
	for _, result := range resp.Results {
		klog.Infof("Result: %+v\n", result)
	}
}