package instagram

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"regexp"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/govdbot/govd/internal/database"
	"github.com/govdbot/govd/internal/logger"
	"github.com/govdbot/govd/internal/models"
	"github.com/govdbot/govd/internal/networking"
	"github.com/govdbot/govd/internal/util"
)

var instagramHost = []string{"instagram", "ddinstagram"}

var Extractor = &models.Extractor{
	ID:          "instagram",
	DisplayName: "Instagram",

	URLPattern: regexp.MustCompile(`https:\/\/(www\.)?(?:dd)?instagram\.com\/(reels?|p|tv)\/(?P<id>[a-zA-Z0-9_-]+)`),
	Host:       instagramHost,
	Redirect:   false,

	GetFunc: func(ctx *models.ExtractorContext) (*models.ExtractorResponse, error) {
		// method 0: private api. needs a session, but it's the only one
		// that returns video urls, and it depends on no persisted query id
		if HasSession(ctx) {
			media, err := GetPrivateAPIMedia(ctx)
			if err == nil {
				ctx.Debugf("extracted via private api")
				return &models.ExtractorResponse{
					Media: media,
				}, nil
			}
			ctx.Debugf("private api failed: %v", err)
		}
		// method 1: get media from GQL web API
		media, err1 := GetGQLMedia(ctx)
		if err1 == nil {
			ctx.Debugf("extracted via gql")
			return &models.ExtractorResponse{
				Media: media,
			}, nil
		}
		// method 2: get media from embed page
		media, err2 := GetEmbedMedia(ctx)
		if err2 == nil {
			ctx.Debugf("extracted via embed page")
			return &models.ExtractorResponse{
				Media: media,
			}, nil
		}
		// method 3: get media from 3rd party service (unlikely)
		media, err3 := GetIGramPost(ctx)
		if err3 == nil {
			ctx.Debugf("extracted via igram")
			return &models.ExtractorResponse{
				Media: media,
			}, nil
		}
		return nil, fmt.Errorf("all methods failed: %w; %w; %w", err1, err2, err3)
	},
}

var StoriesExtractor = &models.Extractor{
	ID:          "instagram",
	DisplayName: "Instagram Stories",

	URLPattern: regexp.MustCompile(`https:\/\/(www\.)?(?:dd)?instagram\.com\/stories\/[a-zA-Z0-9._]+\/(?P<id>\d+)`),
	Host:       instagramHost,
	Hidden:     true,

	GetFunc: func(ctx *models.ExtractorContext) (*models.ExtractorResponse, error) {
		media, err := GetIGramStory(ctx)
		return &models.ExtractorResponse{
			Media: media,
		}, err
	},
}

var ShareURLExtractor = &models.Extractor{
	ID:          "instagram",
	DisplayName: "Instagram (Share)",

	URLPattern: regexp.MustCompile(`https?:\/\/(www\.)?(?:dd)?instagram\.com\/share\/((reels?|video|s|p)\/)?(?P<id>[^\/\?]+)`),
	Host:       instagramHost,

	Redirect: true,

	GetFunc: func(ctx *models.ExtractorContext) (*models.ExtractorResponse, error) {
		redirectURL, err := ctx.FetchLocation(ctx.ContentURL, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to get url location: %w", err)
		}
		return &models.ExtractorResponse{URL: redirectURL}, nil
	},
}

func GetGQLMedia(ctx *models.ExtractorContext) (*models.Media, error) {
	graphData, err := GetGQLData(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get graph data: %w", err)
	}
	return ParseGQLMedia(ctx, graphData.ShortcodeMedia)
}

func GetPrivateAPIMedia(ctx *models.ExtractorContext) (*models.Media, error) {
	mediaID, err := ShortcodeToMediaID(ctx.ContentID)
	if err != nil {
		return nil, fmt.Errorf("failed to build media id: %w", err)
	}

	headers := map[string]string{
		"x-ig-app-id": instagramAppID,
		"Accept":      "*/*",
		"Referer":     "https://www.instagram.com/p/" + ctx.ContentID + "/",
	}
	resp, err := ctx.Fetch(
		http.MethodGet,
		fmt.Sprintf(privateAPIEndpoint, mediaID),
		&networking.RequestParams{
			Headers: headers,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	logger.WriteFile("ig_private_api_response", resp)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get media info: %s", resp.Status)
	}
	// without a valid session instagram answers with the web app itself
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "json") {
		return nil, fmt.Errorf("session not accepted, got %q", contentType)
	}

	var response PrivateMediaResponse
	decoder := sonic.ConfigFastest.NewDecoder(resp.Body)
	if err := decoder.Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	if len(response.Items) == 0 {
		return nil, fmt.Errorf("no items in response")
	}
	return ParsePrivateMedia(ctx, response.Items[0])
}

func GetEmbedMedia(ctx *models.ExtractorContext) (*models.Media, error) {
	embedURL := fmt.Sprintf(
		"https://www.instagram.com/p/%s/embed/captioned",
		ctx.ContentID,
	)
	resp, err := ctx.Fetch(
		http.MethodGet,
		embedURL,
		&networking.RequestParams{
			Headers: webHeaders,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	logger.WriteFile("ig_embed_response", resp)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get embed page: %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	graphData, err := ParseEmbedGQL(body)
	if err != nil {
		return nil, fmt.Errorf("failed to parse embed page: %w", err)
	}
	return ParseGQLMedia(ctx, graphData)
}

func GetIGramPost(ctx *models.ExtractorContext) (*models.Media, error) {
	details, err := GetPostFromIGram(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get post: %w", err)
	}

	media := ctx.NewMedia()
	for _, obj := range details.Items {
		urlObj, contentURL, err := GetIGramMediaURL(obj.URL)
		if err != nil {
			return nil, err
		}
		// the thumbnail is optional: if igram doesn't provide a usable
		// one, it gets extracted from the video itself later on
		var thumbnailURL string
		if obj.Thumb != "" {
			thumbnailURL, err = GetCDNURL(obj.Thumb)
			if err != nil {
				ctx.Debugf("failed to parse igram thumbnail URL: %v", err)
			}
		}
		fileExt := urlObj.Ext
		formatID := urlObj.Type
		switch fileExt {
		case "mp4":
			item := media.NewItem()
			item.AddFormats(&models.MediaFormat{
				FormatID:     formatID,
				Type:         database.MediaTypeVideo,
				URL:          []string{contentURL},
				VideoCodec:   database.MediaCodecAvc,
				AudioCodec:   database.MediaCodecAac,
				ThumbnailURL: optionalURL(thumbnailURL),
			},
			)
		case "jpg", "png", "webp", "heic", "jpeg":
			item := media.NewItem()
			item.AddFormats(&models.MediaFormat{
				Type:     database.MediaTypePhoto,
				FormatID: formatID,
				URL:      []string{contentURL},
			})
		default:
			return nil, fmt.Errorf("unknown format: %s", fileExt)
		}
	}

	if len(media.Items) == 0 {
		return nil, fmt.Errorf("no media found")
	}

	return media, nil
}

func GetIGramStory(ctx *models.ExtractorContext) (*models.Media, error) {
	details, err := GetStoryFromIGram(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get story: %w", err)
	}

	if len(details.Result) == 0 {
		return nil, util.ErrUnavailable
	}
	result := details.Result[0]
	isVideo := len(result.VideoVersions) > 0

	media := ctx.NewMedia()
	item := media.NewItem()
	if isVideo {
		video := GetBestVideoVersion(result.VideoVersions)
		if video == nil || video.URL == "" {
			return nil, fmt.Errorf("no video url found")
		}
		item.AddFormats(&models.MediaFormat{
			FormatID:   "video",
			Type:       database.MediaTypeVideo,
			URL:        []string{video.URL},
			VideoCodec: database.MediaCodecAvc,
			AudioCodec: database.MediaCodecAac,
		})
	} else {
		if result.ImageVersions == nil {
			return nil, fmt.Errorf("no image versions found")
		}
		image := GetBestCandidate(result.ImageVersions.Candidates)
		if image == nil || image.URL == "" {
			return nil, fmt.Errorf("no image url found")
		}
		item.AddFormats(&models.MediaFormat{
			Type:     database.MediaTypePhoto,
			FormatID: "photo",
			URL:      []string{image.URL},
		})
	}

	if len(media.Items) == 0 {
		return nil, fmt.Errorf("no media found")
	}

	return media, nil
}

func GetPostFromIGram(ctx *models.ExtractorContext) (*IGramResponse, error) {
	contentURL := "https://www.instagram.com/p/" + ctx.ContentID + "/"
	apiURL := fmt.Sprintf("https://%s/api/convert", igramHostname)
	payload, err := IGramBodyFromURL(contentURL)
	if err != nil {
		return nil, fmt.Errorf("failed to build signed payload: %w", err)
	}

	headers := map[string]string{
		"Content-Type": "application/json",
	}
	maps.Copy(headers, igramHeaders)

	resp, err := ctx.Fetch(
		http.MethodPost,
		apiURL,
		&networking.RequestParams{
			Body:    payload,
			Headers: headers,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	logger.WriteFile("ig_3party_response", resp)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get response: %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	response, err := ParseIGramResponse(body)
	if err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return response, nil
}

func GetStoryFromIGram(ctx *models.ExtractorContext) (*IGramStoryResponse, error) {
	apiURL := fmt.Sprintf("https://%s/api/v1/instagram/story", igramHostname)
	payload, err := IGramBodyFromParams(map[string]string{
		"url": ctx.ContentURL,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to build signed payload: %w", err)
	}

	headers := map[string]string{
		"Content-Type": "application/json",
	}
	maps.Copy(headers, igramHeaders)

	resp, err := ctx.Fetch(
		http.MethodPost,
		apiURL,
		&networking.RequestParams{
			Body:    payload,
			Headers: headers,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	logger.WriteFile("ig_story_3party_response", resp)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get response: %s", resp.Status)
	}

	var story IGramStoryResponse
	decoder := sonic.ConfigFastest.NewDecoder(resp.Body)
	err = decoder.Decode(&story)
	if err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &story, nil
}
