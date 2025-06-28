package service

import (
	"context"
	"errors"
	"fmt"
	"krillin-ai/internal/dto"
	"krillin-ai/internal/storage"
	"krillin-ai/internal/types"
	"krillin-ai/log"
	"krillin-ai/pkg/util"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/samber/lo"
	"go.uber.org/zap"
)

func (s Service) validateVideoURL(url string) error {
	if strings.Contains(url, "youtube.com") {
		videoId, _ := util.GetYouTubeID(url)
		if videoId == "" {
			return fmt.Errorf("invalid YouTube URL")
		}
	}
	if strings.Contains(url, "bilibili.com") {
		videoId := util.GetBilibiliVideoId(url)
		if videoId == "" {
			return fmt.Errorf("invalid Bilibili URL")
		}
	}
	return nil
}

func (s Service) StartSubtitleTask(req dto.StartVideoSubtitleTaskReq) (*dto.StartVideoSubtitleTaskResData, error) {
	// Validate URL using shared function
	if err := s.validateVideoURL(req.Url); err != nil {
		return nil, err
	}

	// 生成任务id
	seperates := strings.Split(req.Url, "/")
	taskId := fmt.Sprintf("%s_%s", util.SanitizePathName(string([]rune(strings.ReplaceAll(seperates[len(seperates)-1], " ", ""))[:16])), util.GenerateRandStringWithUpperLowerNum(4))
	taskId = strings.ReplaceAll(taskId, "=", "") // 等于号影响ffmpeg处理
	// 构造任务所需参数
	var resultType types.SubtitleResultType
	// 根据入参选项确定要返回的字幕类型
	if req.TargetLang == "none" {
		resultType = types.SubtitleResultTypeOriginOnly
	} else {
		if req.Bilingual == types.SubtitleTaskBilingualYes {
			if req.TranslationSubtitlePos == types.SubtitleTaskTranslationSubtitlePosTop {
				resultType = types.SubtitleResultTypeBilingualTranslationOnTop
			} else {
				resultType = types.SubtitleResultTypeBilingualTranslationOnBottom
			}
		} else {
			resultType = types.SubtitleResultTypeTargetOnly
		}
	}
	// 文字替换map
	replaceWordsMap := make(map[string]string)
	if len(req.Replace) > 0 {
		for _, replace := range req.Replace {
			beforeAfter := strings.Split(replace, "|")
			if len(beforeAfter) == 2 {
				replaceWordsMap[beforeAfter[0]] = beforeAfter[1]
			} else {
				log.GetLogger().Info("generateAudioSubtitles replace param length err", zap.Any("replace", replace), zap.Any("taskId", taskId))
			}
		}
	}
	var err error
	ctx := context.Background()
	// 创建字幕任务文件夹
	taskBasePath := filepath.Join("./tasks", taskId)
	if _, err = os.Stat(taskBasePath); os.IsNotExist(err) {
		// 不存在则创建
		err = os.MkdirAll(filepath.Join(taskBasePath, "output"), os.ModePerm)
		if err != nil {
			log.GetLogger().Error("StartVideoSubtitleTask MkdirAll err", zap.Any("req", req), zap.Error(err))
		}
	}

	// 创建任务
	taskPtr := &types.SubtitleTask{
		TaskId:   taskId,
		VideoSrc: req.Url,
		Status:   types.SubtitleTaskStatusProcessing,
	}
	storage.SubtitleTasks.Store(taskId, taskPtr)

	// 处理声音克隆源
	var voiceCloneAudioUrl string
	if req.TtsVoiceCloneSrcFileUrl != "" {
		localFileUrl := strings.TrimPrefix(req.TtsVoiceCloneSrcFileUrl, "local:")
		fileKey := util.GenerateRandStringWithUpperLowerNum(5) + filepath.Ext(localFileUrl) // 防止url encode的问题，这里统一处理
		err = s.OssClient.UploadFile(context.Background(), fileKey, localFileUrl, s.OssClient.Bucket)
		if err != nil {
			log.GetLogger().Error("StartVideoSubtitleTask UploadFile err", zap.Any("req", req), zap.Error(err))
			return nil, errors.New("上传声音克隆源失败")
		}
		voiceCloneAudioUrl = fmt.Sprintf("https://%s.oss-cn-shanghai.aliyuncs.com/%s", s.OssClient.Bucket, fileKey)
		log.GetLogger().Info("StartVideoSubtitleTask 上传声音克隆源成功", zap.Any("oss url", voiceCloneAudioUrl))
	}

	stepParam := types.SubtitleTaskStepParam{
		TaskId:                  taskId,
		TaskPtr:                 taskPtr,
		TaskBasePath:            taskBasePath,
		Link:                    req.Url,
		SubtitleResultType:      resultType,
		EnableModalFilter:       req.ModalFilter == types.SubtitleTaskModalFilterYes,
		EnableTts:               req.Tts == types.SubtitleTaskTtsYes,
		TtsVoiceCode:            req.TtsVoiceCode,
		VoiceCloneAudioUrl:      voiceCloneAudioUrl,
		ReplaceWordsMap:         replaceWordsMap,
		OriginLanguage:          types.StandardLanguageCode(req.OriginLanguage),
		TargetLanguage:          types.StandardLanguageCode(req.TargetLang),
		UserUILanguage:          types.StandardLanguageCode(req.Language),
		EmbedSubtitleVideoType:  req.EmbedSubtitleVideoType,
		VerticalVideoMajorTitle: req.VerticalMajorTitle,
		VerticalVideoMinorTitle: req.VerticalMinorTitle,
		MaxWordOneLine:          12, // 默认值
	}
	if req.OriginLanguageWordOneLine != 0 {
		stepParam.MaxWordOneLine = req.OriginLanguageWordOneLine
	}

	log.GetLogger().Info("current task info", zap.String("taskId", taskId), zap.Any("param", stepParam))

	go func() {
		defer func() {
			if r := recover(); r != nil {
				const size = 64 << 10
				buf := make([]byte, size)
				buf = buf[:runtime.Stack(buf, false)]
				log.GetLogger().Error("autoVideoSubtitle panic", zap.Any("panic:", r), zap.Any("stack:", buf))
				stepParam.TaskPtr.Status = types.SubtitleTaskStatusFailed
			}
		}()
		// 新版流程：链接->本地音频文件->视频信息获取（若有）->本地字幕文件->语言合成->视频合成->字幕文件链接生成
		log.GetLogger().Info("video subtitle start task", zap.String("taskId", taskId))
		err = s.linkToFile(ctx, &stepParam)
		if err != nil {
			log.GetLogger().Error("StartVideoSubtitleTask linkToFile err", zap.Any("req", req), zap.Error(err))
			stepParam.TaskPtr.Status = types.SubtitleTaskStatusFailed
			stepParam.TaskPtr.FailReason = err.Error()
			return
		}
		// 暂时不加视频信息
		//err = s.getVideoInfo(ctx, &stepParam)
		//if err != nil {
		//	log.GetLogger().Error("StartVideoSubtitleTask getVideoInfo err", zap.Any("req", req), zap.Error(err))
		//	stepParam.TaskPtr.Status = types.SubtitleTaskStatusFailed
		//	stepParam.TaskPtr.FailReason = "get video info error"
		//	return
		//}
		err = s.audioToSubtitle(ctx, &stepParam)
		if err != nil {
			log.GetLogger().Error("StartVideoSubtitleTask audioToSubtitle err", zap.Any("req", req), zap.Error(err))
			stepParam.TaskPtr.Status = types.SubtitleTaskStatusFailed
			stepParam.TaskPtr.FailReason = err.Error()
			return
		}
		err = s.srtFileToSpeech(ctx, &stepParam)
		if err != nil {
			log.GetLogger().Error("StartVideoSubtitleTask srtFileToSpeech err", zap.Any("req", req), zap.Error(err))
			stepParam.TaskPtr.Status = types.SubtitleTaskStatusFailed
			stepParam.TaskPtr.FailReason = err.Error()
			return
		}
		err = s.embedSubtitles(ctx, &stepParam)
		if err != nil {
			log.GetLogger().Error("StartVideoSubtitleTask embedSubtitles err", zap.Any("req", req), zap.Error(err))
			stepParam.TaskPtr.Status = types.SubtitleTaskStatusFailed
			stepParam.TaskPtr.FailReason = err.Error()
			return
		}
		err = s.uploadSubtitles(ctx, &stepParam)
		if err != nil {
			log.GetLogger().Error("StartVideoSubtitleTask uploadSubtitles err", zap.Any("req", req), zap.Error(err))
			stepParam.TaskPtr.Status = types.SubtitleTaskStatusFailed
			stepParam.TaskPtr.FailReason = err.Error()
			return
		}

		log.GetLogger().Info("video subtitle task end", zap.String("taskId", taskId))
	}()

	return &dto.StartVideoSubtitleTaskResData{
		TaskId: taskId,
	}, nil
}

func (s Service) GetTaskStatus(req dto.GetVideoSubtitleTaskReq) (*dto.GetVideoSubtitleTaskResData, error) {
	task, ok := storage.SubtitleTasks.Load(req.TaskId)
	if !ok || task == nil {
		return nil, errors.New("任务不存在")
	}
	taskPtr := task.(*types.SubtitleTask)
	if taskPtr.Status == types.SubtitleTaskStatusFailed {
		return nil, fmt.Errorf("任务失败，原因：%s", taskPtr.FailReason)
	}
	return &dto.GetVideoSubtitleTaskResData{
		TaskId:         taskPtr.TaskId,
		ProcessPercent: taskPtr.ProcessPct,
		VideoInfo: &dto.VideoInfo{
			Title:                 taskPtr.Title,
			Description:           taskPtr.Description,
			TranslatedTitle:       taskPtr.TranslatedTitle,
			TranslatedDescription: taskPtr.TranslatedDescription,
			Language:              taskPtr.OriginLanguage,
		},
		SubtitleInfo: lo.Map(taskPtr.SubtitleInfos, func(item types.SubtitleInfo, _ int) *dto.SubtitleInfo {
			return &dto.SubtitleInfo{
				Name:        item.Name,
				DownloadUrl: item.DownloadUrl,
			}
		}),
		TargetLanguage:    taskPtr.TargetLanguage,
		SpeechDownloadUrl: taskPtr.SpeechDownloadUrl,
	}, nil
}

func (s Service) TranscribeVideo(req dto.TranscribeVideoReq) (*dto.TranscribeVideoResData, error) {
	// Validate URL using shared function
	if err := s.validateVideoURL(req.Url); err != nil {
		return nil, err
	}

	// Generate temporary task ID
	taskId := "transcribe_" + util.GenerateRandStringWithUpperLowerNum(8)

	// Create temporary directory
	taskBasePath := filepath.Join("./temp", taskId)
	if err := os.MkdirAll(taskBasePath, os.ModePerm); err != nil {
		return nil, fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(taskBasePath) // Clean up after processing

	ctx := context.Background()

	// Step 1: Download video and extract audio
	audioFilePath, err := s.downloadAndExtractAudio(ctx, req.Url, taskBasePath)
	if err != nil {
		return nil, fmt.Errorf("failed to download and extract audio: %w", err)
	}

	// Step 2: Transcribe audio to get subtitles
	transcriptionData, err := s.Transcriber.Transcription(audioFilePath, req.OriginLanguage, taskBasePath)
	if err != nil {
		return nil, fmt.Errorf("failed to transcribe audio: %w", err)
	}

	// Step 3: Extract text without timestamps
	subtitles := s.extractTextWithoutTimestamps(transcriptionData)

	return &dto.TranscribeVideoResData{
		Subtitles: subtitles,
		Language:  req.OriginLanguage,
	}, nil
}

func (s Service) downloadAndExtractAudio(ctx context.Context, url, taskBasePath string) (string, error) {
	var localFilePath string

	// Check if it's a local file
	if strings.HasPrefix(url, "local:") {
		localFilePath = strings.TrimPrefix(url, "local:")
	} else {
		// Download video using yt-dlp
		outputPath := filepath.Join(taskBasePath, "video")
		cmd := exec.Command(storage.YtdlpPath,
			"-f", "best[height<=720]", // Limit to 720p to reduce download time
			"-o", outputPath+".%(ext)s",
			url)

		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("failed to download video: %w", err)
		}

		// Find the downloaded file
		files, err := filepath.Glob(outputPath + ".*")
		if err != nil || len(files) == 0 {
			return "", fmt.Errorf("failed to find downloaded video file")
		}
		localFilePath = files[0]
	}

	// Extract audio using ffmpeg
	audioFilePath := filepath.Join(taskBasePath, "audio.mp3")
	cmd := exec.Command(storage.FfmpegPath,
		"-i", localFilePath,
		"-vn", // No video
		"-acodec", "mp3",
		"-ar", "16000", // Sample rate
		"-ac", "1", // Mono
		"-y", // Overwrite
		audioFilePath)

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to extract audio: %w", err)
	}

	return audioFilePath, nil
}

func (s Service) extractTextWithoutTimestamps(transcriptionData *types.TranscriptionData) string {
	if transcriptionData == nil {
		return ""
	}

	// Return the full text without timestamps
	return strings.TrimSpace(transcriptionData.Text)
}
