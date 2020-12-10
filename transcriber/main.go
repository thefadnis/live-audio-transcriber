// refrence taken from google speech streaming API docs
// https://cloud.google.com/speech-to-text/docs/how-to

package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	speech "cloud.google.com/go/speech/apiv1p1beta1"
	redis "github.com/go-redis/redis/v7"
	speechpb "google.golang.org/genproto/googleapis/cloud/speech/v1p1beta1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
)

func resetWordIndex() {
	lastIndex = 0
	pending = nil
}

// Consumes queued audio data from Redis, and sends to Speech.
func dispatchClip(ctx context.Context, speechClient *speech.Client, config speechpb.StreamingRecognitionConfig) {
	var stream speechpb.Speech_StreamingRecognizeClient
	receiveChan := make(chan bool)
	recover := redisClient.Exists(*rQ).Val() > 0
	replay := false

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
				resetWordIndex()
				continue
			}
		default:
			// Process audio
		}

		var res string
		var err error
		if recover {
			res, err = redisClient.RPop(*rQ).Result()
			if err == redis.Nil { // no more recovery data
				recover = false
				replay = false
				continue
			}
			if err == nil && !replay {
				replay = true
				publish("[REPLAY]")
			}
		} else {
			res, err = redisClient.BRPopLPush(*aQ, *rQ, 5*time.Second).Result()
			if err == redis.Nil { // pop timeout
				continue
			}
			// Temporarily retain the last few seconds of recent audio
			if err == nil {
				redisClient.LTrim(*rQ, 0, recoveryRetainSize)
				redisClient.Expire(*rQ, *expiryTime)
			}
		}
		if err != nil && err != redis.Nil {
			klog.Errorf("Could not read from Redis: %v", err)
			recover = true
			continue
		}

		// start a go routine to listen for responses
		if stream == nil {
			stream = initStream(ctx, speechClient, config)
			go consumeAPIresponse(stream, receiveChan)
			publish("[NEW STREAM]")
		}

		// Send audio, transcription responses received asynchronously
		decoded, _ := base64.StdEncoding.DecodeString(res)
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
func consumeAPIresponse(stream speechpb.Speech_StreamingRecognizeClient, receiveChan chan bool) {
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
		treatAPIresponse(*resp)
	}
}

// Handles transcription results.
func treatAPIresponse(resp speechpb.StreamingRecognizeResponse) {
	if len(resp.Results) == 0 {
		return
	}
	res := resp.Results[0]
	alt := res.Alternatives[0]
	newTranscription = alt.Transcript
	elements := strings.Split(alt.Transcript, " ")
	length := len(elements)

	if res.GetIsFinal() || alt.GetConfidence() > 0 {
		klog.Info("Final result! Resetting")
		final := elements[lastIndex:]
		publish(strings.Join(final, " "))
		return
	}
	if res.Stability < 0.75 {
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
		yeildTrancriptAlternatives([]string{}, pending, unstable)
	} else if lastIndex < length-*pendingWordCount {
		steady := elements[lastIndex:(length - *pendingWordCount)]
		lastIndex += len(steady)
		pending = elements[lastIndex:]
		yeildTrancriptAlternatives(steady, pending, unstable)
	}
}

func initStream(ctx context.Context, client *speech.Client, config speechpb.StreamingRecognitionConfig) speechpb.Speech_StreamingRecognizeClient {
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

func yeildTrancriptAlternatives(steady []string, pending []string, unstable string) {
	msg := fmt.Sprintf("%s|%s|%s", strings.Join(steady, " "),
		strings.Join(pending, " "), unstable)
	publish(msg)
}

func publish(msg string) {
	klog.Info(msg)
	redisClient.LPush(*tQ, msg)
}

func flush() {
	msg := ""
	if pending != nil {
		msg += strings.Join(pending, " ")
	}
	if msg != "" {
		klog.Info("Flushing...")
		publish("[FLUSH] " + msg)
	}
	resetWordIndex()
}

// Debugging
func logResponses(resp speechpb.StreamingRecognizeResponse) {
	for _, res := range resp.Results {
		klog.Infof("Result: %+v\n", res)
	}
}

var (
	redisHost          = flag.String("redisHost", "localhost", "Redis IP")
	redisClient        *redis.Client
	aQ         		= flag.String("aQ", "liveq", "input audio data")
	tQ 				= flag.String("tQ", "transcriptions", "output transcriptions")
	rQ      		= flag.String("rQ", "recoverq", "recent audio data")
	retainTime     	= flag.Duration("recoveryRetainLast", 3*time.Second, "Retain  duration ")
	expiryTime     	= flag.Duration("expiryTime", 30*time.Second, "Expire data ")
	flushTimeout       = flag.Duration("flushTimeout", 3000*time.Millisecond, "pending transcriptions publishted after time")
	pendingWordCount   = flag.Int("pendingWordCount", 4, "Treat last N are pending")
	sampleRate         = flag.Int("sampleRate", 16000, "(Hz)")
	channels           = flag.Int("channels", 1, "# audio channels")
	lang               = flag.String("lang", "en-US", "language code")
	phrases            = flag.String("phrases", "", " hints for Speech API")

	recoveryRetainSize = int64(*retainTime / (100 * time.Millisecond)) // each audio element ~100ms
	newTranscription   string
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

	dispatchClip(context.Background(), speechClient, streamingConfig)
}
