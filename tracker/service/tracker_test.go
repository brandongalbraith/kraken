package service

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"testing"

	"code.uber.internal/infra/kraken/config/tracker"
	"code.uber.internal/infra/kraken/testutils"
	"code.uber.internal/infra/kraken/tracker/storage"
	"code.uber.internal/infra/kraken/utils"

	"code.uber.internal/infra/kraken/test/mocks/mock_storage"
	"github.com/golang/mock/gomock"
	bencode "github.com/jackpal/bencode-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testMocks struct {
	appCfg    config.AppConfig
	ctrl      *gomock.Controller
	datastore *mock_storage.MockStorage
}

// mockController sets up all mocks and returns a teardown func that can be called with defer
func (m *testMocks) mockController(t gomock.TestReporter) func() {
	m.appCfg = config.AppConfig{
		PeerHandoutPolicy: config.PeerHandoutConfig{
			Priority: "default",
			Sampling: "default",
		},
	}
	m.ctrl = gomock.NewController(t)
	m.datastore = mock_storage.NewMockStorage(m.ctrl)
	return func() {
		m.ctrl.Finish()
	}
}

func (m *testMocks) CreateHandler() http.Handler {
	return InitializeAPI(
		m.appCfg,
		m.datastore,
	)
}

func (m *testMocks) CreateHandlerAndServeRequest(request *http.Request) *http.Response {
	w := httptest.NewRecorder()
	m.CreateHandler().ServeHTTP(w, request)
	return w.Result()
}

func performRequest(handler http.Handler, request *http.Request) *http.Response {
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request)
	return w.Result()
}

var (
	peer                *storage.PeerInfo
	torrent             *storage.TorrentInfo
	announceRequestPath string
)

func TestMain(m *testing.M) {
	torrent = storage.TorrentFixture()
	peer = storage.PeerForTorrentFixture(torrent)

	rawinfoHash, err := hex.DecodeString(torrent.InfoHash)
	if err != nil {
		panic(err)
	}
	rawpeerID, err := hex.DecodeString(peer.PeerID)
	if err != nil {
		panic(err)
	}

	v := url.Values{}
	v.Set("info_hash", string(rawinfoHash))
	v.Set("peer_id", string(rawpeerID))
	v.Set("ip", strconv.Itoa(int(utils.IPtoInt32(net.ParseIP(peer.IP)))))
	v.Set("port", strconv.FormatInt(peer.Port, 10))
	v.Set("dc", peer.DC)
	v.Set("downloaded", strconv.FormatInt(peer.BytesDownloaded, 10))
	v.Set("uploaded", strconv.FormatInt(peer.BytesUploaded, 10))
	v.Set("left", strconv.FormatInt(peer.BytesLeft, 10))
	v.Set("event", peer.Event)

	announceRequestPath = "/announce?" + v.Encode()

	os.Exit(m.Run())
}

func TestAnnounceEndPoint(t *testing.T) {

	t.Run("Return 500 if missing parameters", func(t *testing.T) {
		announceRequest, _ := http.NewRequest("GET", "/announce", nil)

		mocks := &testMocks{}
		defer mocks.mockController(t)()

		response := mocks.CreateHandlerAndServeRequest(announceRequest)
		require.Equal(t, 500, response.StatusCode)
	})
	t.Run("Return 200 and empty bencoded response", func(t *testing.T) {

		announceRequest, _ := http.NewRequest("GET", announceRequestPath, nil)

		mocks := &testMocks{}
		defer mocks.mockController(t)()

		mocks.datastore.EXPECT().Read(torrent.InfoHash).Return([]*storage.PeerInfo{}, nil)
		mocks.datastore.EXPECT().Update(peer).Return(nil)
		response := mocks.CreateHandlerAndServeRequest(announceRequest)
		require.Equal(t, 200, response.StatusCode)
		announceResponse := AnnouncerResponse{}
		bencode.Unmarshal(response.Body, &announceResponse)
		assert.Equal(t, announceResponse.Interval, int64(0))
		assert.Equal(t, announceResponse.Peers, []storage.PeerInfo{})
	})
	t.Run("Return 200 and single peer bencoded response", func(t *testing.T) {

		announceRequest, _ := http.NewRequest("GET", announceRequestPath, nil)
		mocks := &testMocks{}
		defer mocks.mockController(t)()

		peerTo := &storage.PeerInfo{
			PeerID: peer.PeerID,
			IP:     peer.IP,
			Port:   peer.Port}

		mocks.datastore.EXPECT().Read(torrent.InfoHash).Return([]*storage.PeerInfo{peer}, nil)
		mocks.datastore.EXPECT().Update(peer).Return(nil)
		response := mocks.CreateHandlerAndServeRequest(announceRequest)
		testutils.RequireStatus(t, response, 200)
		announceResponse := AnnouncerResponse{}
		bencode.Unmarshal(response.Body, &announceResponse)
		assert.Equal(t, announceResponse.Interval, int64(0))
		assert.Equal(t, announceResponse.Peers, []storage.PeerInfo{*peerTo})
	})

}

func TestGetInfoHashHandler(t *testing.T) {
	infoHash := "12345678901234567890"
	name := "asdfhjkl"

	t.Run("Return 400 on empty name", func(t *testing.T) {
		getRequest, _ := http.NewRequest("GET",
			"/infohash?name=", nil)

		mocks := &testMocks{}
		defer mocks.mockController(t)()
		getResponse := mocks.CreateHandlerAndServeRequest(getRequest)
		assert.Equal(t, 400, getResponse.StatusCode)
	})

	t.Run("Return 404 on name not found", func(t *testing.T) {
		getRequest, _ := http.NewRequest("GET",
			"/infohash?name="+name, nil)

		mocks := &testMocks{}
		defer mocks.mockController(t)()

		mocks.datastore.EXPECT().ReadTorrent(name).Return(nil, nil)
		response := mocks.CreateHandlerAndServeRequest(getRequest)
		assert.Equal(t, 404, response.StatusCode)
	})

	t.Run("Return 200 and info hash", func(t *testing.T) {
		getRequest, _ := http.NewRequest("GET",
			"/infohash?name="+name, nil)

		mocks := &testMocks{}
		defer mocks.mockController(t)()

		mocks.datastore.EXPECT().ReadTorrent(name).Return(&storage.TorrentInfo{InfoHash: infoHash}, nil)
		response := mocks.CreateHandlerAndServeRequest(getRequest)
		assert.Equal(t, 200, response.StatusCode)
		data := make([]byte, len(infoHash))
		response.Body.Read(data)
		assert.Equal(t, infoHash, string(data[:]))
	})
}

func TestPostInfoHashHandler(t *testing.T) {
	infoHash := "12345678901234567890"
	name := "asdfhjkl"

	t.Run("Return 400 on empty name or infohash", func(t *testing.T) {
		getRequest, _ := http.NewRequest("POST",
			"/infohash?name=", nil)

		mocks := &testMocks{}
		defer mocks.mockController(t)()
		getResponse := mocks.CreateHandlerAndServeRequest(getRequest)
		assert.Equal(t, 400, getResponse.StatusCode)

		getRequest, _ = http.NewRequest("POST",
			"/infohash?name="+name, nil)
		getResponse = mocks.CreateHandlerAndServeRequest(getRequest)
		assert.Equal(t, 400, getResponse.StatusCode)
	})

	t.Run("Return 200", func(t *testing.T) {
		getRequest, _ := http.NewRequest("POST",
			"/infohash?name="+name+"&info_hash="+infoHash, nil)

		mocks := &testMocks{}
		defer mocks.mockController(t)()

		mocks.datastore.EXPECT().CreateTorrent(&storage.TorrentInfo{
			TorrentName: name,
			InfoHash:    infoHash,
		}).Return(nil)
		response := mocks.CreateHandlerAndServeRequest(getRequest)
		assert.Equal(t, 200, response.StatusCode)
	})
}

func TestPostManifestHandler(t *testing.T) {
	manifest := `{
                 "schemaVersion": 2,
                 "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
                 "config": {
                    "mediaType": "application/octet-stream",
                    "size": 11936,
                    "digest": "sha256:d2176faa6180566e5e6727e101ba26b13c19ef35f171c9b4419c4d50626aad9d"
                 },
                 "layers": [{
                    "mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
                    "size": 52998821,
                    "digest": "sha256:1508613826413590a9fdb496cbedb0c2ebf564cfbcd2c85c2a07bb3a40813233"
                 },
                 {
                    "mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
                    "size": 115242848,
                    "digest": "sha256:f1f1d5da237f1b069eae23cdc9b291e217a4c1fda8f29262c4275a786a4dd322"
                  }]}`
	name := "tag1"

	t.Run("Return 400 on invalid manifest", func(t *testing.T) {
		getRequest, _ := http.NewRequest("POST",
			"/manifest/"+name, bytes.NewBuffer([]byte("")))

		mocks := &testMocks{}
		defer mocks.mockController(t)()
		getResponse := mocks.CreateHandlerAndServeRequest(getRequest)
		assert.Equal(t, 400, getResponse.StatusCode)
	})

	t.Run("Return 200", func(t *testing.T) {
		getRequest, _ := http.NewRequest("POST",
			"/manifest/"+name, bytes.NewBuffer([]byte(manifest)))

		mocks := &testMocks{}
		defer mocks.mockController(t)()

		mocks.datastore.EXPECT().UpdateManifest(&storage.Manifest{
			TagName:  name,
			Manifest: manifest,
			Flags:    0,
		}).Return(nil)
		response := mocks.CreateHandlerAndServeRequest(getRequest)
		assert.Equal(t, 200, response.StatusCode)
	})
}

// JSONBytesEqual compares the JSON in two byte slices.
func JSONBytesEqual(a, b []byte) (bool, error) {
	var j1, j2 interface{}
	if err := json.Unmarshal(a, &j1); err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, &j2); err != nil {
		return false, err
	}
	return reflect.DeepEqual(j2, j1), nil
}

func TestGetManifestHandler(t *testing.T) {
	manifest := `{
                 "schemaVersion": 2,
                 "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
                 "config": {
                    "mediaType": "application/octet-stream",
                    "size": 11936,
                    "digest": "sha256:d2176faa6180566e5e6727e101ba26b13c19ef35f171c9b4419c4d50626aad9d"
                 },
                 "layers": [{
                    "mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
                    "size": 52998821,
                    "digest": "sha256:1508613826413590a9fdb496cbedb0c2ebf564cfbcd2c85c2a07bb3a40813233"
                 },
                 {
                    "mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
                    "size": 115242848,
                    "digest": "sha256:f1f1d5da237f1b069eae23cdc9b291e217a4c1fda8f29262c4275a786a4dd322"
                  }]}`
	name := "repo:tag1"

	t.Run("Return 400 on empty tag name", func(t *testing.T) {
		getRequest, _ := http.NewRequest("GET",
			"/manifest/", nil)

		mocks := &testMocks{}
		defer mocks.mockController(t)()
		getResponse := mocks.CreateHandlerAndServeRequest(getRequest)
		assert.Equal(t, 400, getResponse.StatusCode)
	})

	t.Run("Return 404 on manifest not found", func(t *testing.T) {
		getRequest, _ := http.NewRequest("GET",
			"/manifest/"+name, nil)

		mocks := &testMocks{}
		defer mocks.mockController(t)()

		mocks.datastore.EXPECT().ReadManifest(name).Return(nil, nil)
		response := mocks.CreateHandlerAndServeRequest(getRequest)
		assert.Equal(t, 404, response.StatusCode)
	})

	t.Run("Return 200 and manifest", func(t *testing.T) {
		getRequest, _ := http.NewRequest("GET",
			"/manifest/"+name, nil)

		mocks := &testMocks{}
		defer mocks.mockController(t)()

		mocks.datastore.EXPECT().ReadManifest(name).Return(
			&storage.Manifest{TagName: name, Manifest: manifest}, nil)
		response := mocks.CreateHandlerAndServeRequest(getRequest)
		assert.Equal(t, 200, response.StatusCode)
		data, _ := ioutil.ReadAll(response.Body)
		var j1, j2 interface{}
		j1, err := json.Marshal(data)
		assert.Equal(t, err, nil)
		err = json.Unmarshal([]byte(manifest), &j2)
		assert.Equal(t, err, nil)

		result := reflect.DeepEqual(j1, j1)
		assert.Equal(t, result, true)
	})
}
