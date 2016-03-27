// Package elastictranscoder provides a implementation of the provider that
// uses AWS Elastic Transcoder for transcoding media files.
//
// It doesn't expose any public type. In order to use the provider, one must
// import this package and then grab the factory from the provider package:
//
//     import (
//         "github.com/nytm/video-transcoding-api/provider"
//         "github.com/nytm/video-transcoding-api/provider/elastictranscoder"
//     )
//
//     func UseProvider() {
//         factory, err := provider.GetProviderFactory(elastictranscoder.Name)
//         // handle err and use factory to get an instance of the provider.
//     }
package elastictranscoder

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/elastictranscoder"
	"github.com/aws/aws-sdk-go/service/elastictranscoder/elastictranscoderiface"
	"github.com/nytm/video-transcoding-api/config"
	"github.com/nytm/video-transcoding-api/db"
	"github.com/nytm/video-transcoding-api/provider"
)

const (
	// Name is the name used for registering the Elastic Transcoder
	// provider in the registry of providers.
	Name = "elastictranscoder"

	defaultAWSRegion = "us-east-1"
)

var (
	errAWSInvalidConfig = errors.New("invalid Elastic Transcoder config. Please define the configuration entries in the config file or environment variables")
	s3Pattern           = regexp.MustCompile(`^s3://`)
)

func init() {
	provider.Register(Name, elasticTranscoderProvider)
}

type awsProvider struct {
	c      elastictranscoderiface.ElasticTranscoderAPI
	config *config.ElasticTranscoder
}

func (p *awsProvider) Transcode(transcodeProfile provider.TranscodeProfile) (*provider.JobStatus, error) {
	var adaptiveStreaming bool
	if transcodeProfile.StreamingParams.Protocol == "hls" {
		adaptiveStreaming = true
	}
	source := p.normalizeSource(transcodeProfile.SourceMedia)
	params := elastictranscoder.CreateJobInput{
		PipelineId: aws.String(p.config.PipelineID),
		Input:      &elastictranscoder.JobInput{Key: aws.String(source)},
	}
	params.Outputs = make([]*elastictranscoder.CreateJobOutput, len(transcodeProfile.Presets))
	for i, preset := range transcodeProfile.Presets {
		presetID, ok := preset.ProviderMapping[Name]
		if !ok {
			return nil, provider.ErrPresetNotFound
		}
		params.Outputs[i] = &elastictranscoder.CreateJobOutput{
			PresetId: aws.String(presetID),
			Key:      p.outputKey(preset.OutputOpts, source, preset.Name, adaptiveStreaming),
		}
		if adaptiveStreaming {
			params.Outputs[i].SegmentDuration = aws.String(strconv.Itoa(int(transcodeProfile.StreamingParams.SegmentDuration)))
		}
	}

	if adaptiveStreaming {
		jobPlaylist := elastictranscoder.CreateJobPlaylist{
			Format: aws.String("HLSv3"),
			Name:   aws.String(strings.TrimRight(source, filepath.Ext(source)) + "/master"),
		}

		jobPlaylist.OutputKeys = make([]*string, len(transcodeProfile.Presets))
		for i, preset := range transcodeProfile.Presets {
			jobPlaylist.OutputKeys[i] = p.outputKey(preset.OutputOpts, source, preset.Name, adaptiveStreaming)
		}

		params.Playlists = []*elastictranscoder.CreateJobPlaylist{&jobPlaylist}
	}

	resp, err := p.c.CreateJob(&params)
	if err != nil {
		return nil, err
	}
	outputDestination, err := p.getOutputDestination(resp.Job)
	if err != nil {
		outputDestination = []string{
			err.Error(),
		}
	}
	return &provider.JobStatus{
		ProviderName:      Name,
		ProviderJobID:     aws.StringValue(resp.Job.Id),
		Status:            provider.StatusQueued,
		OutputDestination: outputDestination,
	}, nil
}

func (p *awsProvider) normalizeSource(source string) string {
	if s3Pattern.MatchString(source) {
		source = strings.Replace(source, "s3://", "", 1)
		parts := strings.SplitN(source, "/", 2)
		return parts[len(parts)-1]
	}
	return source
}

func (p *awsProvider) outputKey(opts db.OutputOptions, source, presetName string, adaptiveStreaming bool) *string {
	parts := strings.Split(source, "/")
	lastIndex := len(parts) - 1
	fileName := parts[lastIndex]
	if adaptiveStreaming {
		fileName = strings.TrimRight(fileName, filepath.Ext(fileName))
		parts = append(parts[0:lastIndex], fileName, presetName, "video")
	} else {
		fileName = strings.TrimRight(fileName, filepath.Ext(fileName)) + "." + strings.TrimLeft(opts.Extension, ".")
		parts = append(parts[0:lastIndex], presetName, fileName)
	}
	return aws.String(strings.Join(parts, "/"))
}

func (p *awsProvider) JobStatus(id string) (*provider.JobStatus, error) {
	resp, err := p.c.ReadJob(&elastictranscoder.ReadJobInput{Id: aws.String(id)})
	if err != nil {
		return nil, err
	}
	outputs := make(map[string]interface{})
	for _, output := range resp.Job.Outputs {
		outputs[aws.StringValue(output.Key)] = aws.StringValue(output.StatusDetail)
	}
	outputDestination, err := p.getOutputDestination(resp.Job)
	if err != nil {
		outputDestination = []string{
			err.Error(),
		}
	}
	return &provider.JobStatus{
		ProviderJobID:     aws.StringValue(resp.Job.Id),
		Status:            p.statusMap(aws.StringValue(resp.Job.Status)),
		ProviderStatus:    map[string]interface{}{"outputs": outputs},
		OutputDestination: outputDestination,
	}, nil
}

func (p *awsProvider) getOutputDestination(job *elastictranscoder.Job) ([]string, error) {
	var outputDestination []string
	readPipelineOutput, err := p.c.ReadPipeline(&elastictranscoder.ReadPipelineInput{
		Id: job.PipelineId,
	})
	if err != nil {
		return nil, err
	}
	outputKeyPrefix := ""
	if job.OutputKeyPrefix != nil {
		outputKeyPrefix = *job.OutputKeyPrefix
	}
	for _, playlist := range job.Playlists {
		if *playlist.Format == "HLSv3" {
			// master playlist
			outputDestination = append(outputDestination,
				fmt.Sprintf("s3://%s/%s%s",
					*readPipelineOutput.Pipeline.OutputBucket,
					*playlist.Name,
					".m3u8",
				),
			)
			for _, output := range job.Outputs {
				// Get preset playlist
				outputDestination = append(outputDestination,
					fmt.Sprintf("s3://%s/%s%s%s",
						*readPipelineOutput.Pipeline.OutputBucket,
						outputKeyPrefix,
						*output.Key,
						".m3u8",
					),
				)
				if output.DurationMillis == nil || output.SegmentDuration == nil {
					continue
				}
				// Infer .ts segment file names from total duration
				// returned by final step in transcoding process
				// Number of segments = total duration / segment duration
				outputDurationMillis := float64(*output.DurationMillis)
				segmentDuration, _ := strconv.ParseFloat(*output.SegmentDuration, 64)
				numSegments := int(outputDurationMillis/segmentDuration/1000 + 0.5)
				for segmentIndex := 0; segmentIndex < numSegments; segmentIndex++ {
					outputDestination = append(outputDestination,
						fmt.Sprintf("s3://%s/%s%s%s%s",
							*readPipelineOutput.Pipeline.OutputBucket,
							outputKeyPrefix,
							*output.Key,
							fmt.Sprintf("%05d", segmentIndex),
							".ts",
						),
					)
				}
			}
		}
	}
	// If this is not an adaptive streaming job, just parse filenames
	// from plain outputs
	if len(job.Playlists) == 0 {
		for _, output := range job.Outputs {
			outputDestination = append(outputDestination,
				fmt.Sprintf("s3://%s/%s%s",
					*readPipelineOutput.Pipeline.OutputBucket,
					outputKeyPrefix,
					*output.Key,
				),
			)
		}
	}
	return outputDestination, nil
}

func (p *awsProvider) statusMap(awsStatus string) provider.Status {
	switch awsStatus {
	case "Submitted":
		return provider.StatusQueued
	case "Progressing":
		return provider.StatusStarted
	case "Complete":
		return provider.StatusFinished
	case "Canceled":
		return provider.StatusCanceled
	default:
		return provider.StatusFailed
	}
}

func (p *awsProvider) Healthcheck() error {
	_, err := p.c.ReadPipeline(&elastictranscoder.ReadPipelineInput{
		Id: aws.String(p.config.PipelineID),
	})
	return err
}

func (p *awsProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		InputFormats:  []string{"h264"},
		OutputFormats: []string{"mp4", "hls", "webm"},
		Destinations:  []string{"s3"},
	}
}

func elasticTranscoderProvider(cfg *config.Config) (provider.TranscodingProvider, error) {
	if cfg.ElasticTranscoder.AccessKeyID == "" || cfg.ElasticTranscoder.SecretAccessKey == "" || cfg.ElasticTranscoder.PipelineID == "" {
		return nil, errAWSInvalidConfig
	}
	creds := credentials.NewStaticCredentials(cfg.ElasticTranscoder.AccessKeyID, cfg.ElasticTranscoder.SecretAccessKey, "")
	region := cfg.ElasticTranscoder.Region
	if region == "" {
		region = defaultAWSRegion
	}
	awsSession := session.New(aws.NewConfig().WithCredentials(creds).WithRegion(region))
	return &awsProvider{
		c:      elastictranscoder.New(awsSession),
		config: cfg.ElasticTranscoder,
	}, nil
}
