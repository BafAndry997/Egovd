package instagram

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/govdbot/govd/internal/database"
	"github.com/govdbot/govd/internal/logger"
	"github.com/govdbot/govd/internal/models"
	"github.com/govdbot/govd/internal/networking"
	"github.com/govdbot/govd/internal/util"

	"github.com/bytedance/sonic"
	"github.com/titanous/json5"
)

const (
	graphQLEndpoint = "https://www.instagram.com/graphql/query/"
	polarisAction   = "PolarisPostActionLoadPostQueryQuery"

	privateAPIEndpoint = "https://www.instagram.com/api/v1/media/%s/info/"
	instagramAppID     = "936619743392459"

	// shortcodes are the media id written in this alphabet
	shortcodeAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

	// media_type values returned by the private api
	privateTypePhoto    = 1
	privateTypeVideo    = 2
	privateTypeCarousel = 8

	igramHostname = "api-wh.igram.world"
	igramAPIBase  = "api.igram.world"
	igramHMACKey  = "75f2d70d3724f98e4a7d1ffd0ba9cfd907f3ae2632ee159980e2c521bff62358"
	igramStaticTS = 1771418815381 // parseInt("mls10xp1", 36)
)

var (
	embedPattern = regexp.MustCompile(
		`new ServerJS\(\)\);s\.handle\(({.*})\);requireLazy`)

	webHeaders = map[string]string{
		"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
		"Accept-Language":           "en-GB,en;q=0.9",
		"Cache-Control":             "max-age=0",
		"Dnt":                       "1",
		"Priority":                  "u=0, i",
		"Sec-Ch-Ua":                 `Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99`,
		"Sec-Ch-Ua-Mobile":          "?0",
		"Sec-Ch-Ua-Platform":        "macOS",
		"Sec-Fetch-Dest":            "document",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-Site":            "none",
		"Sec-Fetch-User":            "?1",
		"Upgrade-Insecure-Requests": "1",
	}

	igramHeaders = map[string]string{
		"Referer": "https://igram.world/",
	}
)

func ParseGQLMedia(ctx *models.ExtractorContext, data *Media) (*models.Media, error) {
	var caption string
	if data.EdgeMediaToCaption != nil && len(data.EdgeMediaToCaption.Edges) > 0 {
		if node := data.EdgeMediaToCaption.Edges[0].Node; node != nil {
			caption = node.Text
		}
	}

	media := ctx.NewMedia()
	media.SetCaption(caption)

	// the item is created only once the format is known to be usable, so
	// that a partially broken response doesn't produce empty items
	addMediaFormat := func(format *models.MediaFormat) {
		if len(format.URL) == 0 || format.URL[0] == "" {
			return
		}
		item := media.NewItem()
		item.AddFormats(format)
	}

	switch data.Typename {
	case "GraphVideo", "XDTGraphVideo":
		width, height := mediaDimensions(data.Dimensions)
		addMediaFormat(&models.MediaFormat{
			FormatID:     "video",
			Type:         database.MediaTypeVideo,
			VideoCodec:   database.MediaCodecAvc,
			AudioCodec:   database.MediaCodecAac,
			URL:          []string{data.VideoURL},
			ThumbnailURL: optionalURL(data.DisplayURL),
			Width:        width,
			Height:       height,
		})
	case "GraphImage", "XDTGraphImage":
		addMediaFormat(&models.MediaFormat{
			FormatID: "image",
			Type:     database.MediaTypePhoto,
			URL:      []string{data.DisplayURL},
		})
	case "GraphSidecar", "XDTGraphSidecar":
		if data.EdgeSidecarToChildren != nil && len(data.EdgeSidecarToChildren.Edges) > 0 {
			edges := data.EdgeSidecarToChildren.Edges

			for i := range edges {
				node := edges[i].Node
				if node == nil {
					continue
				}

				// a child that can't be resolved fails the whole album: an
				// unauthenticated response carries no video_url, and serving
				// just the photos would silently drop part of the carousel.
				// failing here lets the next extraction method try instead
				switch node.Typename {
				case "GraphVideo", "XDTGraphVideo":
					if node.VideoURL == "" {
						return nil, fmt.Errorf("no video_url for sidecar child %d", i)
					}
					width, height := mediaDimensions(node.Dimensions)
					addMediaFormat(&models.MediaFormat{
						FormatID:     "video",
						Type:         database.MediaTypeVideo,
						VideoCodec:   database.MediaCodecAvc,
						AudioCodec:   database.MediaCodecAac,
						URL:          []string{node.VideoURL},
						ThumbnailURL: optionalURL(node.DisplayURL),
						Width:        width,
						Height:       height,
					})

				case "GraphImage", "XDTGraphImage":
					if node.DisplayURL == "" {
						return nil, fmt.Errorf("no display_url for sidecar child %d", i)
					}
					addMediaFormat(&models.MediaFormat{
						FormatID: "image",
						Type:     database.MediaTypePhoto,
						URL:      []string{node.DisplayURL},
					})
				}
			}
		}
	}

	// an empty media would be treated as a success by the caller,
	// preventing the other extraction methods from being tried
	if len(media.Items) == 0 {
		return nil, fmt.Errorf("no playable media found for %s", data.Typename)
	}

	return media, nil
}

// returns a nil slice for empty urls, so that callers
// don't end up with a []string{""} to download
func optionalURL(contentURL string) []string {
	if contentURL == "" {
		return nil
	}
	return []string{contentURL}
}

// missing dimensions are filled in later by probing the file itself
func mediaDimensions(dimensions *Dimensions) (int32, int32) {
	if dimensions == nil {
		return 0, 0
	}
	return dimensions.Width, dimensions.Height
}

func ParseEmbedGQL(body []byte) (*Media, error) {
	match := embedPattern.FindSubmatch(body)
	if len(match) < 2 {
		return nil, fmt.Errorf("gql json not found")
	}
	jsonData := match[1]

	var data map[string]any
	if err := json5.Unmarshal(jsonData, &data); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON: %w", err)
	}
	igCtx := util.TraverseJSON(data, "contextJSON")
	if igCtx == nil {
		return nil, fmt.Errorf("contextJSON not found")
	}
	var ctxJSON ContextJSON
	switch v := igCtx.(type) {
	case string:
		if err := json5.Unmarshal([]byte(v), &ctxJSON); err != nil {
			return nil, fmt.Errorf("failed to unmarshal contextJSON: %w", err)
		}
	default:
		return nil, fmt.Errorf("unexpected type for contextJSON: %T", v)
	}
	if ctxJSON.GqlData == nil {
		return nil, fmt.Errorf("gql_data not found")
	}
	if ctxJSON.GqlData.ShortcodeMedia == nil {
		return nil, fmt.Errorf("shortcode_media not found")
	}
	return ctxJSON.GqlData.ShortcodeMedia, nil
}

func IGramBodyFromURL(contentURL string) (io.Reader, error) {
	return igramBuildPayload(map[string]string{
		"target_url": contentURL,
	})
}

func IGramBodyFromParams(params map[string]string) (io.Reader, error) {
	return igramBuildPayload(params)
}

func igramBuildPayload(urlParams map[string]string) (io.Reader, error) {
	nowMs := time.Now().UnixMilli()
	serverMs := getIGramServerTime()

	drift := serverMs - nowMs
	var correction int64
	if drift >= 60000 || drift <= -60000 {
		correction = drift
	}
	ts := nowMs + correction

	// partial payload fields that get signed
	partial := map[string]any{
		"_sc": 0,
		"_ef": 0,
		"_df": 0,
	}
	for k, v := range urlParams {
		partial[k] = v
	}

	sig, err := igramSign(partial, ts)
	if err != nil {
		return nil, err
	}

	// assemble final payload
	final := make(map[string]any, len(partial)+5)
	for k, v := range partial {
		final[k] = v
	}
	final["ts"] = ts
	final["_ts"] = igramStaticTS
	final["_tsc"] = correction
	final["_sv"] = 2
	final["_s"] = sig

	jsonBytes, err := sonic.ConfigFastest.Marshal(final)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payload: %w", err)
	}

	return strings.NewReader(string(jsonBytes)), nil
}

func igramSign(partial map[string]any, ts int64) (string, error) {
	// sonic.ConfigStd sorts map keys alphabetically, matching
	// the signing: JSON.stringify(sorted_partial) + String(ts)
	jsonBytes, err := sonic.ConfigStd.Marshal(partial)
	if err != nil {
		return "", fmt.Errorf("failed to marshal partial payload: %w", err)
	}

	data := string(jsonBytes) + strconv.FormatInt(ts, 10)

	keyBytes, err := hex.DecodeString(igramHMACKey)
	if err != nil {
		return "", fmt.Errorf("failed to decode HMAC key: %w", err)
	}

	mac := hmac.New(sha256.New, keyBytes)
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func getIGramServerTime() int64 {
	apiURL := fmt.Sprintf("https://%s/msec", igramAPIBase)
	resp, err := http.Get(apiURL)
	if err != nil {
		return time.Now().UnixMilli()
	}
	defer resp.Body.Close()

	var result struct {
		Msec float64 `json:"msec"`
	}
	decoder := sonic.ConfigFastest.NewDecoder(resp.Body)
	if err := decoder.Decode(&result); err != nil {
		return time.Now().UnixMilli()
	}
	return int64(result.Msec * 1000)
}

func ParseIGramResponse(body []byte) (*IGramResponse, error) {
	// try to unmarshal as a single IGramMedia and then as a slice
	var media IGramMedia

	if err := sonic.ConfigFastest.Unmarshal(body, &media); err != nil {
		// try with slice
		var mediaList []*IGramMedia
		if err := sonic.ConfigFastest.Unmarshal(body, &mediaList); err != nil {
			return nil, fmt.Errorf("failed to decode response: %w", err)
		}
		return &IGramResponse{
			Items: mediaList,
		}, nil
	}
	if media.Success != nil && !(*media.Success) {
		return nil, util.ErrUnavailable
	}
	return &IGramResponse{
		Items: []*IGramMedia{&media},
	}, nil
}

// picks the first usable entry, as igram may return
// several of them and the first one isn't always valid
func GetIGramMediaURL(urls []*IGramMediaURL) (*IGramMediaURL, string, error) {
	if len(urls) == 0 {
		return nil, "", fmt.Errorf("no media url found")
	}
	var lastErr error
	for _, urlObj := range urls {
		if urlObj == nil || urlObj.URL == "" {
			continue
		}
		contentURL, err := GetCDNURL(urlObj.URL)
		if err != nil {
			lastErr = err
			continue
		}
		return urlObj, contentURL, nil
	}
	if lastErr != nil {
		return nil, "", fmt.Errorf("no valid media url found: %w", lastErr)
	}
	return nil, "", fmt.Errorf("no valid media url found")
}

func GetCDNURL(contentURL string) (string, error) {
	contentURL, parsedURL, err := normalizeIGramURL(contentURL)
	if err != nil {
		return "", err
	}
	queryParams, err := url.ParseQuery(parsedURL.RawQuery)
	if err != nil {
		return "", fmt.Errorf("can't unescape igram URL: %w", err)
	}
	if cdnURL := queryParams.Get("uri"); cdnURL != "" {
		cdnURL, _, err = normalizeIGramURL(cdnURL)
		if err != nil {
			return "", err
		}
		return cdnURL, nil
	}
	// igram may hand out the cdn link directly,
	// without wrapping it in the uri query param
	return contentURL, nil
}

func normalizeIGramURL(rawURL string) (string, *url.URL, error) {
	trimmedURL := strings.TrimSpace(rawURL)
	parsedURL, err := url.Parse(trimmedURL)
	if err != nil {
		return "", nil, fmt.Errorf("can't parse igram URL: %w", err)
	}
	// protocol-relative urls (//host/path) parse without a scheme
	if parsedURL.Scheme == "" && parsedURL.Host != "" {
		parsedURL.Scheme = "https"
		trimmedURL = parsedURL.String()
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return "", nil, fmt.Errorf("invalid igram URL scheme: %q", parsedURL.Scheme)
	}
	return trimmedURL, parsedURL, nil
}

func GetGQLData(ctx *models.ExtractorContext) (*GraphQLData, error) {
	var sessionCookies []*http.Cookie
	if ctx.HTTPClient != nil {
		sessionCookies = ctx.HTTPClient.Cookies
	}
	sessionCSRF, sessionUserID := InstagramSession(sessionCookies)

	graphHeaders, body, err := BuildGQLData(sessionCSRF, sessionUserID)
	if err != nil {
		return nil, fmt.Errorf("failed to build GQL data: %w", err)
	}
	formData := url.Values{}
	for key, value := range body {
		formData.Set(key, value)
	}
	formData.Set("fb_api_caller_class", "RelayModern")
	formData.Set("fb_api_req_friendly_name", polarisAction)
	variables := map[string]any{
		"shortcode":               ctx.ContentID,
		"fetch_tagged_user_count": nil,
		"hoisted_comment_id":      nil,
		"hoisted_reply_id":        nil,
	}
	variablesJSON, err := sonic.ConfigFastest.Marshal(variables)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal variables: %w", err)
	}
	formData.Set("variables", string(variablesJSON))
	formData.Set("server_timestamps", "true")
	formData.Set("doc_id", "8845758582119845") // idk what this is

	for key, value := range webHeaders {
		graphHeaders[key] = value
	}
	resp, err := ctx.Fetch(
		http.MethodPost,
		graphQLEndpoint,
		&networking.RequestParams{
			Headers: graphHeaders,
			Body:    strings.NewReader(formData.Encode()),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	logger.WriteFile("iggql_api_response", resp)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("invalid response code: %s", resp.Status)
	}
	var response GraphQLResponse
	decoder := sonic.ConfigFastest.NewDecoder(resp.Body)
	if err := decoder.Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	if response.Data == nil {
		return nil, fmt.Errorf("data is nil")
	}
	if response.Status != "ok" {
		return nil, fmt.Errorf("status is not ok: %s", response.Status)
	}
	if response.Data.ShortcodeMedia == nil {
		return nil, fmt.Errorf("shortcode_media is nil")
	}
	return response.Data, nil
}

// the shortcode is the media id encoded in base64 over a custom alphabet,
// so the private api id is derived locally, without an extra request
func ShortcodeToMediaID(shortcode string) (string, error) {
	if shortcode == "" {
		return "", fmt.Errorf("empty shortcode")
	}
	mediaID := new(big.Int)
	base := big.NewInt(int64(len(shortcodeAlphabet)))
	for _, char := range shortcode {
		index := strings.IndexRune(shortcodeAlphabet, char)
		if index < 0 {
			return "", fmt.Errorf("invalid character %q in shortcode", char)
		}
		mediaID.Mul(mediaID, base)
		mediaID.Add(mediaID, big.NewInt(int64(index)))
	}
	return mediaID.String(), nil
}

// reports whether a logged in session was loaded from the cookie file
func HasSession(ctx *models.ExtractorContext) bool {
	if ctx.HTTPClient == nil {
		return false
	}
	_, userID := InstagramSession(ctx.HTTPClient.Cookies)
	return userID != ""
}

func ParsePrivateMedia(ctx *models.ExtractorContext, item *PrivateMediaItem) (*models.Media, error) {
	if item == nil {
		return nil, fmt.Errorf("empty media item")
	}
	media := ctx.NewMedia()
	if item.Caption != nil {
		media.SetCaption(item.Caption.Text)
	}

	children := []*PrivateMediaItem{item}
	if item.MediaType == privateTypeCarousel {
		children = item.CarouselMedia
	}
	for i, child := range children {
		format, err := privateMediaFormat(child)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
		media.NewItem().AddFormats(format)
	}

	if len(media.Items) == 0 {
		return nil, fmt.Errorf("no playable media found")
	}
	return media, nil
}

func privateMediaFormat(item *PrivateMediaItem) (*models.MediaFormat, error) {
	if item == nil {
		return nil, fmt.Errorf("empty media item")
	}
	switch item.MediaType {
	case privateTypeVideo:
		video := GetBestVideoVersion(item.VideoVersions)
		if video == nil || video.URL == "" {
			return nil, fmt.Errorf("no video url found")
		}
		return &models.MediaFormat{
			FormatID:     "video",
			Type:         database.MediaTypeVideo,
			VideoCodec:   database.MediaCodecAvc,
			AudioCodec:   database.MediaCodecAac,
			URL:          []string{video.URL},
			ThumbnailURL: optionalURL(bestCandidateURL(item.ImageVersions)),
			Width:        int32(video.Width),
			Height:       int32(video.Height),
		}, nil
	case privateTypePhoto:
		photoURL := bestCandidateURL(item.ImageVersions)
		if photoURL == "" {
			return nil, fmt.Errorf("no image url found")
		}
		return &models.MediaFormat{
			FormatID: "photo",
			Type:     database.MediaTypePhoto,
			URL:      []string{photoURL},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported media type: %d", item.MediaType)
	}
}

func bestCandidateURL(versions *ImageVersions) string {
	if versions == nil {
		return ""
	}
	candidate := GetBestCandidate(versions.Candidates)
	if candidate == nil {
		return ""
	}
	return candidate.URL
}

// extracts the values the graphql endpoint needs to recognise a session, out
// of the cookies loaded from private/cookies/instagram.txt. both are empty
// when no cookie file is present, which keeps the anonymous behaviour.
func InstagramSession(cookies []*http.Cookie) (csrfToken string, userID string) {
	for _, cookie := range cookies {
		if cookie == nil {
			continue
		}
		switch cookie.Name {
		case "csrftoken":
			csrfToken = cookie.Value
		case "ds_user_id":
			userID = cookie.Value
		}
	}
	return csrfToken, userID
}

func BuildGQLData(sessionCSRF string, sessionUserID string) (map[string]string, map[string]string, error) {
	const (
		domain                = "www"
		requestID             = "b"
		clientCapabilityGrade = "EXCELLENT"
		sessionInternalID     = "7436540909012459023"
		apiVersion            = "1"
		rolloutHash           = "1019933358"
		bloksVersionID        = "6309c8d03d8a3f47a1658ba38b304a3f837142ef5f637ebf1f8f52d4b802951e"
		asbdID                = "129477"
		hiddenState           = "20126.HYP:instagram_web_pkg.2.1...0"
		loggedIn              = "0"
		cometRequestID        = "7"
		appVersion            = "0"
		pixelRatio            = "2"
		buildType             = "trunk"
	)
	session := "::" + util.RandomAlphaString(6)
	sessionData := util.RandomBase64(8)
	csrfToken := util.RandomBase64(32)
	deviceID := util.RandomBase64(24)
	machineID := util.RandomBase64(24)
	dynamicFlags := util.RandomBase64(154)
	clientSessionRnd := util.RandomBase64(154)
	jazoestBig, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate jazoest: %w", err)
	}
	jazoest := strconv.FormatInt(jazoestBig.Int64()+1, 10)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	cookies := []string{
		"csrftoken=" + csrfToken,
		"ig_did=" + deviceID,
		"wd=1280x720",
		"dpr=2",
		"mid=" + machineID,
		"ig_nrcb=1",
	}
	headers := map[string]string{
		"x-ig-app-id":        instagramAppID,
		"X-FB-LSD":           sessionData,
		"X-CSRFToken":        csrfToken,
		"X-Bloks-Version-Id": bloksVersionID,
		"x-asbd-id":          asbdID,
		"cookie":             strings.Join(cookies, "; "),
		"Content-Type":       "application/x-www-form-urlencoded",
		"X-FB-Friendly-Name": polarisAction,
	}
	if sessionCSRF != "" {
		// a session is available: drop the anonymous cookie header, which
		// would otherwise overwrite the real cookies attached by the http
		// client, and keep the csrf token consistent with them
		delete(headers, "cookie")
		headers["X-CSRFToken"] = sessionCSRF
	}
	body := map[string]string{
		"__d":         domain,
		"__a":         apiVersion,
		"__s":         session,
		"__hs":        hiddenState,
		"__req":       requestID,
		"__ccg":       clientCapabilityGrade,
		"__rev":       rolloutHash,
		"__hsi":       sessionInternalID,
		"__dyn":       dynamicFlags,
		"__csr":       clientSessionRnd,
		"__user":      loggedIn,
		"__comet_req": cometRequestID,
		"libav":       appVersion,
		"dpr":         pixelRatio,
		"lsd":         sessionData,
		"jazoest":     jazoest,
		"__spin_r":    rolloutHash,
		"__spin_b":    buildType,
		"__spin_t":    timestamp,
	}
	if sessionUserID != "" {
		// instagram expects the request to declare the logged in user
		body["__user"] = sessionUserID
	}
	return headers, body, nil
}

func GetBestCandidate(candidates []*Candidates) *Candidates {
	if len(candidates) == 0 {
		return nil
	}
	best := candidates[0]
	for _, candidate := range candidates {
		if candidate.Width > best.Width {
			best = candidate
		}
	}
	return best
}

func GetBestVideoVersion(versions []*VideoVersions) *VideoVersions {
	if len(versions) == 0 {
		return nil
	}
	best := versions[0]
	for _, version := range versions {
		if version.Width > best.Width {
			best = version
		}
	}
	return best
}
