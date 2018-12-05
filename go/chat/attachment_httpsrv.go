package chat

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/keybase/client/go/chat/attachments"
	"github.com/keybase/client/go/chat/globals"
	"github.com/keybase/client/go/chat/s3"
	"github.com/keybase/client/go/chat/types"
	"github.com/keybase/client/go/chat/utils"
	"github.com/keybase/client/go/kbhttp"
	"github.com/keybase/client/go/libkb"
	disklru "github.com/keybase/client/go/lru"
	"github.com/keybase/client/go/protocol/chat1"
	"github.com/keybase/client/go/protocol/gregor1"
	"github.com/keybase/client/go/protocol/keybase1"
	"golang.org/x/net/context"
)

var blankProgress = func(bytesComplete, bytesTotal int64) {}

type AttachmentHTTPSrv struct {
	sync.Mutex
	globals.Contextified
	utils.DebugLabeler

	endpoint        string
	pendingEndpoint string
	unfurlEndpoint  string
	httpSrv         *kbhttp.Srv
	urlMap          *lru.Cache
	unfurlMap       *lru.Cache
	fetcher         types.AttachmentFetcher
	ri              func() chat1.RemoteInterface
}

var _ types.AttachmentURLSrv = (*AttachmentHTTPSrv)(nil)

func NewAttachmentHTTPSrv(g *globals.Context, fetcher types.AttachmentFetcher, ri func() chat1.RemoteInterface) *AttachmentHTTPSrv {
	l, err := lru.New(10000)
	if err != nil {
		panic(err)
	}
	um, err := lru.New(200)
	if err != nil {
		panic(err)
	}
	r := &AttachmentHTTPSrv{
		Contextified:    globals.NewContextified(g),
		DebugLabeler:    utils.NewDebugLabeler(g.GetLog(), "AttachmentHTTPSrv", false),
		endpoint:        "at",
		pendingEndpoint: "pe",
		unfurlEndpoint:  "uf",
		ri:              ri,
		urlMap:          l,
		unfurlMap:       um,
		fetcher:         fetcher,
	}
	r.initHTTPSrv()
	r.startHTTPSrv()
	g.PushShutdownHook(func() error {
		r.httpSrv.Stop()
		return nil
	})
	go r.monitorAppState()

	return r
}

func (r *AttachmentHTTPSrv) monitorAppState() {
	ctx := context.Background()
	r.Debug(ctx, "monitorAppState: starting up")
	state := keybase1.AppState_FOREGROUND
	for {
		state = <-r.G().AppState.NextUpdate(&state)
		switch state {
		case keybase1.AppState_FOREGROUND:
			r.startHTTPSrv()
		case keybase1.AppState_BACKGROUND, keybase1.AppState_INACTIVE:
			r.httpSrv.Stop()
		}
	}
}

func (r *AttachmentHTTPSrv) initHTTPSrv() {
	startPort := r.G().GetEnv().GetAttachmentHTTPStartPort()
	r.httpSrv = kbhttp.NewSrv(r.G().GetLog(), kbhttp.NewPortRangeListenerSource(startPort, 18000))
}

func (r *AttachmentHTTPSrv) startHTTPSrv() {
	maxTries := 2
	success := false
	for i := 0; i < maxTries; i++ {
		if err := r.httpSrv.Start(); err != nil {
			if err == kbhttp.ErrPinnedPortInUse {
				// If we hit this, just try again and get a different port.
				// The advantage is that backing in and out of the thread will restore attachments,
				// whereas if we do nothing you need to bkg/foreground.
				r.Debug(context.TODO(),
					"startHTTPSrv: pinned port taken error, re-initializing and trying again")
				r.initHTTPSrv()
				continue
			}
			r.Debug(context.TODO(), "startHTTPSrv: failed to start HTTP server: %s", err)
			break
		}
		success = true
		break
	}
	if !success {
		r.Debug(context.TODO(), "startHTTPSrv: exhausted attempts to start HTTP server, giving up")
		return
	}
	r.httpSrv.HandleFunc("/"+r.endpoint, r.serve)
	r.httpSrv.HandleFunc("/"+r.pendingEndpoint, r.servePendingPreview)
	r.httpSrv.HandleFunc("/"+r.unfurlEndpoint, r.serveUnfurlAsset)
}

func (r *AttachmentHTTPSrv) GetAttachmentFetcher() types.AttachmentFetcher {
	return r.fetcher
}

func (r *AttachmentHTTPSrv) randURLKey(prefix string) (string, error) {
	return libkb.RandHexString(prefix, 8)
}

func (r *AttachmentHTTPSrv) GetURL(ctx context.Context, convID chat1.ConversationID, msgID chat1.MessageID,
	preview bool) string {
	r.Lock()
	defer r.Unlock()
	defer r.Trace(ctx, func() error { return nil }, "GetURL(%s,%d)", convID, msgID)()
	if !r.httpSrv.Active() {
		r.Debug(ctx, "GetURL: http server failed to start earlier")
		return ""
	}
	addr, err := r.httpSrv.Addr()
	if err != nil {
		r.Debug(ctx, "GetURL: failed to get HTTP server address: %s", err)
		return ""
	}
	key, err := r.randURLKey("at")
	if err != nil {
		r.Debug(ctx, "GetURL: failed to generate URL key: %s", err)
		return ""
	}
	r.urlMap.Add(key, chat1.ConversationIDMessageIDPair{
		ConvID: convID,
		MsgID:  msgID,
	})
	url := fmt.Sprintf("http://%s/%s?key=%s&prev=%v", addr, r.endpoint, key, preview)
	r.Debug(ctx, "GetURL: handler URL: convID: %s msgID: %d %s", convID, msgID, url)
	return url
}

func (r *AttachmentHTTPSrv) GetPendingPreviewURL(ctx context.Context, outboxID chat1.OutboxID) string {
	defer r.Trace(ctx, func() error { return nil }, "GetPendingPreviewURL(%s)", outboxID)()
	addr, err := r.httpSrv.Addr()
	if err != nil {
		r.Debug(ctx, "GetPendingPreviewURL: failed to get HTTP server address: %s", err)
		return ""
	}
	url := fmt.Sprintf("http://%s/%s?key=%s", addr, r.pendingEndpoint, outboxID)
	r.Debug(ctx, "GetPendingPreviewURL: handler URL: outboxID: %s %s", outboxID, url)
	return url
}

type unfurlAsset struct {
	asset  chat1.Asset
	convID chat1.ConversationID
}

func (r *AttachmentHTTPSrv) GetUnfurlAssetURL(ctx context.Context, convID chat1.ConversationID,
	asset chat1.Asset) string {
	defer r.Trace(ctx, func() error { return nil }, "GetUnfurlAssetURL")()
	addr, err := r.httpSrv.Addr()
	if err != nil {
		r.Debug(ctx, "GetUnfurlAssetURL: failed to get HTTP server address: %s", err)
		return ""
	}
	key, err := r.randURLKey("uf")
	if err != nil {
		r.Debug(ctx, "GetURL: failed to generate URL key: %s", err)
		return ""
	}
	r.unfurlMap.Add(key, unfurlAsset{
		asset:  asset,
		convID: convID,
	})
	url := fmt.Sprintf("http://%s/%s?key=%s", addr, r.unfurlEndpoint, key)
	r.Debug(ctx, "GetUnfurlAssetURL: handler URL: %s", url)
	return url
}

func (r *AttachmentHTTPSrv) servePendingPreview(w http.ResponseWriter, req *http.Request) {
	ctx := Context(context.Background(), r.G(), keybase1.TLFIdentifyBehavior_CHAT_GUI, nil,
		NewSimpleIdentifyNotifier(r.G()))
	defer r.Trace(ctx, func() error { return nil }, "servePendingPreview")()
	strOutboxID := req.URL.Query().Get("key")
	outboxID, err := chat1.MakeOutboxID(strOutboxID)
	if err != nil {
		r.makeError(ctx, w, http.StatusBadRequest, "invalid outbox ID: %s", err)
		return
	}
	pre, err := attachments.NewPendingPreviews(r.G()).Get(ctx, outboxID)
	if err != nil {
		r.makeError(ctx, w, http.StatusInternalServerError, "error reading preview: %s", err)
		return
	}
	if _, err := io.Copy(w, bytes.NewReader(pre.Preview)); err != nil {
		r.makeError(ctx, w, http.StatusInternalServerError, "failed to write resposne: %s", err)
		return
	}
}

func (r *AttachmentHTTPSrv) serveUnfurlAsset(w http.ResponseWriter, req *http.Request) {
	ctx := Context(context.Background(), r.G(), keybase1.TLFIdentifyBehavior_CHAT_GUI, nil,
		NewSimpleIdentifyNotifier(r.G()))
	defer r.Trace(ctx, func() error { return nil }, "serveUnfurlAsset")()
	key := req.URL.Query().Get("key")
	val, ok := r.unfurlMap.Get(key)
	if !ok {
		r.makeError(ctx, w, http.StatusInternalServerError, "invalid key: %s", key)
		return
	}
	ua := val.(unfurlAsset)
	if r.shouldServeContent(ctx, ua.asset, req) {
		if r.serveUnfurlVideoHostPage(ctx, w, req) {
			// if we served the host page, just bail out
			return
		}
		r.Debug(ctx, "serveUnfurlAsset: streaming: req: method: %s range: %s", req.Method,
			req.Header.Get("Range"))
		rs, err := r.fetcher.StreamAttachment(ctx, ua.convID, ua.asset, r.ri, r)
		if err != nil {
			r.makeError(ctx, w, http.StatusInternalServerError, "failed to get streamer: %s", err)
			return
		}
		http.ServeContent(w, req, ua.asset.Filename, time.Time{}, rs)
	} else {
		if err := r.fetcher.FetchAttachment(ctx, w, ua.convID, ua.asset, r.ri, r, blankProgress); err != nil {
			r.makeError(ctx, w, http.StatusInternalServerError, "failed to fetch attachment: %s", err)
			return
		}
	}
}

func (r *AttachmentHTTPSrv) makeError(ctx context.Context, w http.ResponseWriter, code int, msg string,
	args ...interface{}) {
	r.Debug(ctx, "serve: error code: %d msg %s", code, fmt.Sprintf(msg, args...))
	w.WriteHeader(code)
}

func (r *AttachmentHTTPSrv) shouldServeContent(ctx context.Context, asset chat1.Asset, req *http.Request) bool {
	noStream := "true" == req.URL.Query().Get("nostream")
	if noStream {
		// If we just want the bits without streaming
		return false
	}
	return strings.HasPrefix(asset.MimeType, "video")
}

func (r *AttachmentHTTPSrv) serveUnfurlVideoHostPage(ctx context.Context, w http.ResponseWriter, req *http.Request) bool {
	contentForce := "true" == req.URL.Query().Get("contentforce")
	if r.G().GetAppType() == libkb.MobileAppType && !contentForce {
		r.Debug(ctx, "serveUnfurlVideoHostPage: mobile client detected, showing the HTML video viewer")
		w.Header().Set("Content-Type", "text/html")
		if _, err := w.Write([]byte(fmt.Sprintf(`
			<html>
				<head>
					<meta name="viewport" content="initial-scale=1, viewport-fit=cover">
					<title>Keybase Video Viewer</title>
					<script>
						window.playVideo = function(data) {
							var vid = document.getElementById("vid");
							vid.play()
						}
					</script>
				</head>
				<body style="margin: 0px; background-color: rgba(0,0,0,0.05)">
					<video id="vid" onloadeddata="playVideo()" style="width: 100%%; height: 100%%; border-radius: 8px; object-fit:fill" src="%s" playsinline webkit-playsinline loop autoplay muted />
				</body>
			</html>
		`, req.URL.String()+"&contentforce=true"))); err != nil {
			r.Debug(ctx, "serveUnfurlVideoHostPage: failed to write HTML video player: %s", err)
		}
		return true
	}
	return false
}

func (r *AttachmentHTTPSrv) serveVideoHostPage(ctx context.Context, w http.ResponseWriter, req *http.Request) bool {
	contentForce := "true" == req.URL.Query().Get("contentforce")
	if r.G().GetAppType() == libkb.MobileAppType && !contentForce {
		r.Debug(ctx, "serve: mobile client detected, showing the HTML video viewer")
		w.Header().Set("Content-Type", "text/html")
		if _, err := w.Write([]byte(fmt.Sprintf(`
			<html>
				<head>
					<title>Keybase Video Viewer</title>
					<script>
						window.togglePlay = function(data) {
							var vid = document.getElementById("vid");
							if (data === "play") {
								vid.play();
								vid.setAttribute('controls', 'controls');
							} else {
								vid.pause();
								vid.removeAttribute('controls');
							}
						  }
					</script>
				</head>
				<body style="margin: 0px; background-color: rgba(0,0,0,0.05)">
					<video id="vid" style="width: 100%%; border-radius: 8px" poster="%s" src="%s" preload="none" playsinline webkit-playsinline />
				</body>
			</html>
		`, req.URL.Query().Get("poster"), req.URL.String()+"&contentforce=true"))); err != nil {
			r.Debug(ctx, "serve: failed to write HTML video player: %s", err)
		}
		return true
	}
	return false
}

func (r *AttachmentHTTPSrv) serve(w http.ResponseWriter, req *http.Request) {
	ctx := Context(context.Background(), r.G(), keybase1.TLFIdentifyBehavior_CHAT_GUI, nil,
		NewSimpleIdentifyNotifier(r.G()))
	defer r.Trace(ctx, func() error { return nil }, "serve")()
	key := req.URL.Query().Get("key")
	preview := false
	if "true" == req.URL.Query().Get("prev") {
		preview = true
	}
	r.Lock()
	pairInt, ok := r.urlMap.Get(key)
	r.Unlock()
	if !ok {
		r.makeError(ctx, w, http.StatusNotFound, "key not found in URL map")
		return
	}

	pair := pairInt.(chat1.ConversationIDMessageIDPair)
	uid := gregor1.UID(r.G().Env.GetUID().ToBytes())
	asset, err := attachments.AssetFromMessage(ctx, r.G(), uid, pair.ConvID, pair.MsgID, preview)
	if err != nil {
		r.makeError(ctx, w, http.StatusInternalServerError, "failed to get asset: %s", err)
		return
	}
	if len(asset.Path) == 0 {
		r.makeError(ctx, w, http.StatusNotFound, "attachment not uploaded yet, no path")
		return
	}
	size := asset.Size
	r.Debug(ctx, "serve: setting content-type: %s sz: %d", asset.MimeType, size)
	w.Header().Set("Content-Type", asset.MimeType)
	if r.shouldServeContent(ctx, asset, req) {
		if r.serveVideoHostPage(ctx, w, req) {
			// if we served the host page, just bail out
			return
		}
		r.Debug(ctx, "serve: streaming: req: method: %s range: %s", req.Method, req.Header.Get("Range"))
		rs, err := r.fetcher.StreamAttachment(ctx, pair.ConvID, asset, r.ri, r)
		if err != nil {
			r.makeError(ctx, w, http.StatusInternalServerError, "failed to get streamer: %s", err)
			return
		}
		http.ServeContent(w, req, asset.Filename, time.Time{}, rs)
	} else {
		if err := r.fetcher.FetchAttachment(ctx, w, pair.ConvID, asset, r.ri, r, blankProgress); err != nil {
			r.makeError(ctx, w, http.StatusInternalServerError, "failed to fetch attachment: %s", err)
			return
		}
	}
}

// Sign implements github.com/keybase/go/chat/s3.Signer interface.
func (r *AttachmentHTTPSrv) Sign(payload []byte) ([]byte, error) {
	arg := chat1.S3SignArg{
		Payload: payload,
		Version: 1,
	}
	return r.ri().S3Sign(context.Background(), arg)
}

type RemoteAttachmentFetcher struct {
	globals.Contextified
	utils.DebugLabeler
	store attachments.Store
}

var _ types.AttachmentFetcher = (*RemoteAttachmentFetcher)(nil)

func NewRemoteAttachmentFetcher(g *globals.Context, store attachments.Store) *RemoteAttachmentFetcher {
	return &RemoteAttachmentFetcher{
		Contextified: globals.NewContextified(g),
		DebugLabeler: utils.NewDebugLabeler(g.GetLog(), "RemoteAttachmentFetcher", false),
		store:        store,
	}
}

func (r *RemoteAttachmentFetcher) StreamAttachment(ctx context.Context, convID chat1.ConversationID,
	asset chat1.Asset, ri func() chat1.RemoteInterface, signer s3.Signer) (res io.ReadSeeker, err error) {
	defer r.Trace(ctx, func() error { return err }, "StreamAttachment")()
	// Grab S3 params for the conversation
	s3params, err := ri().GetS3Params(ctx, convID)
	if err != nil {
		return nil, err
	}
	return r.store.StreamAsset(ctx, s3params, asset, signer)
}

func (r *RemoteAttachmentFetcher) FetchAttachment(ctx context.Context, w io.Writer,
	convID chat1.ConversationID, asset chat1.Asset,
	ri func() chat1.RemoteInterface, signer s3.Signer, progress types.ProgressReporter) (err error) {
	defer r.Trace(ctx, func() error { return err }, "FetchAttachment")()
	// Grab S3 params for the conversation
	s3params, err := ri().GetS3Params(ctx, convID)
	if err != nil {
		return err
	}
	return r.store.DownloadAsset(ctx, s3params, asset, w, signer, progress)
}

func (r *RemoteAttachmentFetcher) DeleteAssets(ctx context.Context,
	convID chat1.ConversationID, assets []chat1.Asset, ri func() chat1.RemoteInterface, signer s3.Signer) (err error) {
	defer r.Trace(ctx, func() error { return err }, "DeleteAssets")()

	if len(assets) == 0 {
		return nil
	}

	// get s3 params from server
	s3params, err := ri().GetS3Params(ctx, convID)
	if err != nil {
		r.Debug(ctx, "error getting s3params: %s", err)
		return err
	}

	// Try to delete the assets remotely
	if err := r.store.DeleteAssets(ctx, s3params, signer, assets); err != nil {
		// there's no way to get asset information after this point.
		// any assets not deleted will be stranded on s3.
		r.Debug(ctx, "error deleting assets: %s", err)
	}

	r.Debug(ctx, "deleted %d assets", len(assets))
	return nil
}

func (r *RemoteAttachmentFetcher) PutUploadedAsset(ctx context.Context, filename string, asset chat1.Asset) error {
	return nil
}

func (r *RemoteAttachmentFetcher) IsAssetLocal(ctx context.Context, asset chat1.Asset) (bool, error) {
	return false, nil
}

type CachingAttachmentFetcher struct {
	globals.Contextified
	utils.DebugLabeler

	store   attachments.Store
	diskLRU *disklru.DiskLRU

	// testing
	tempDir string
}

var _ types.AttachmentFetcher = (*CachingAttachmentFetcher)(nil)

func NewCachingAttachmentFetcher(g *globals.Context, store attachments.Store, size int) *CachingAttachmentFetcher {
	return &CachingAttachmentFetcher{
		Contextified: globals.NewContextified(g),
		DebugLabeler: utils.NewDebugLabeler(g.GetLog(), "CachingAttachmentFetcher", false),
		store:        store,
		diskLRU:      disklru.NewDiskLRU("attachments", 1, size),
	}
}

func (c *CachingAttachmentFetcher) getCacheDir() string {
	if len(c.tempDir) > 0 {
		return c.tempDir
	}
	return filepath.Join(c.G().GetCacheDir(), "attachments")
}

func (c *CachingAttachmentFetcher) getFullFilename(name string) string {
	return name + ".attachment"
}

func (c *CachingAttachmentFetcher) closeFile(f io.Closer) {
	if f != nil {
		f.Close()
	}
}

func (c *CachingAttachmentFetcher) cacheKey(asset chat1.Asset) string {
	return asset.Path
}

func (c *CachingAttachmentFetcher) createAttachmentFile(ctx context.Context) (*os.File, error) {
	os.MkdirAll(c.getCacheDir(), os.ModePerm)
	file, err := ioutil.TempFile(c.getCacheDir(), "att")
	file.Close()
	if err != nil {
		return nil, err
	}
	path := c.getFullFilename(file.Name())
	if err := os.Rename(file.Name(), path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDWR, os.ModeAppend)
}

func (c *CachingAttachmentFetcher) localAssetPath(ctx context.Context, asset chat1.Asset) (found bool, path string, err error) {
	found, entry, err := c.diskLRU.Get(ctx, c.G(), c.cacheKey(asset))
	if err != nil {
		return found, path, err
	}
	if found {
		path = entry.Value.(string)
	}
	return found, path, nil
}

func (c *CachingAttachmentFetcher) StreamAttachment(ctx context.Context, convID chat1.ConversationID,
	asset chat1.Asset, ri func() chat1.RemoteInterface, signer s3.Signer) (res io.ReadSeeker, err error) {
	defer c.Trace(ctx, func() error { return err }, "StreamAttachment")()
	return NewRemoteAttachmentFetcher(c.G(), c.store).StreamAttachment(ctx, convID, asset, ri, signer)
}

func (c *CachingAttachmentFetcher) FetchAttachment(ctx context.Context, w io.Writer,
	convID chat1.ConversationID, asset chat1.Asset, ri func() chat1.RemoteInterface, signer s3.Signer,
	progress types.ProgressReporter) (err error) {

	defer c.Trace(ctx, func() error { return err }, "FetchAttachment")()

	// Check for a disk cache hit, and decrypt that onto the response stream
	found, path, err := c.localAssetPath(ctx, asset)
	if err != nil {
		return err
	}
	if found {
		c.Debug(ctx, "FetchAttachment: cache hit for: %s filepath: %s", asset.Path, path)
		fileReader, err := os.Open(path)
		defer c.closeFile(fileReader)
		if err != nil {
			c.Debug(ctx, "FetchAttachment: failed to read cached file, removing: %s", err)
			os.Remove(path)
			c.diskLRU.Remove(ctx, c.G(), c.cacheKey(asset))
			found = false
		}
		if found {
			return c.store.DecryptAsset(ctx, w, fileReader, asset, progress)
		}
	}

	// Grab S3 params for the conversation
	s3params, err := ri().GetS3Params(ctx, convID)
	if err != nil {
		return err
	}

	// Create a reader to the remote ciphertext
	remoteReader, err := c.store.GetAssetReader(ctx, s3params, asset, signer)
	defer c.closeFile(remoteReader)
	if err != nil {
		return err
	}

	// Create a file we can write the ciphertext into
	fileWriter, err := c.createAttachmentFile(ctx)
	defer c.closeFile(fileWriter)
	if err != nil {
		return err
	}

	// Read out the ciphertext into the decryption copier, and simultaneously write
	// into the cached file (the ciphertext)
	teeReader := io.TeeReader(remoteReader, fileWriter)
	if err := c.store.DecryptAsset(ctx, w, teeReader, asset, progress); err != nil {
		c.Debug(ctx, "FetchAttachment: error reading asset: %s", err)
		c.closeFile(fileWriter)
		os.Remove(fileWriter.Name())
		return err
	}

	// commit to the on disk LRU
	return c.putFileInLRU(ctx, fileWriter.Name(), asset)
}

func (c *CachingAttachmentFetcher) putFileInLRU(ctx context.Context, filename string, asset chat1.Asset) error {
	// Add an entry to the disk LRU mapping the asset path to the local path, and remove
	// the remnants of any evicted attachments.
	evicted, err := c.diskLRU.Put(ctx, c.G(), c.cacheKey(asset), filename)
	if err != nil {
		return err
	}
	if evicted != nil {
		path := evicted.Value.(string)
		os.Remove(path)
	}
	return nil
}

func (c *CachingAttachmentFetcher) IsAssetLocal(ctx context.Context, asset chat1.Asset) (found bool, err error) {
	defer c.Trace(ctx, func() error { return err }, "IsAssetLocal")()
	found, path, err := c.localAssetPath(ctx, asset)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	fileReader, err := os.Open(path)
	defer c.closeFile(fileReader)
	if err != nil {
		return false, nil
	}
	return true, nil
}

func (c *CachingAttachmentFetcher) PutUploadedAsset(ctx context.Context, filename string, asset chat1.Asset) (err error) {
	defer c.Trace(ctx, func() error { return err }, "PutUploadedAsset")()
	return c.putFileInLRU(ctx, filename, asset)
}

func (c *CachingAttachmentFetcher) DeleteAssets(ctx context.Context,
	convID chat1.ConversationID, assets []chat1.Asset, ri func() chat1.RemoteInterface, signer s3.Signer) (err error) {
	defer c.Trace(ctx, func() error { return err }, "DeleteAssets")()

	if len(assets) == 0 {
		return nil
	}

	// Delete the assets locally
	for _, asset := range assets {
		found, path, err := c.localAssetPath(ctx, asset)
		if err != nil {
			c.Debug(ctx, "error getting asset: %s", err)
			continue
		}
		if found {
			os.Remove(path)
			c.diskLRU.Remove(ctx, c.G(), c.cacheKey(asset))
		}
	}

	// get s3 params from server
	s3params, err := ri().GetS3Params(ctx, convID)
	if err != nil {
		c.Debug(ctx, "error getting s3params: %s", err)
		return err
	}

	// Try to delete the assets remotely
	if err := c.store.DeleteAssets(ctx, s3params, signer, assets); err != nil {
		// there's no way to get asset information after this point.
		// any assets not deleted will be stranded on s3.
		c.Debug(ctx, "error deleting assets: %s", err)
	}

	c.Debug(ctx, "deleted %d assets", len(assets))
	return nil
}
