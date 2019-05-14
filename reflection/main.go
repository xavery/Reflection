package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/Workiva/go-datastructures/bitarray"
	"github.com/h31/Reflection/qBT"
	"github.com/h31/Reflection/transmission"
	log "github.com/sirupsen/logrus"
	"hash/fnv"
	"io/ioutil"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

var (
	verbose              = flag.Bool("verbose", false, "Enable verbose output")
	debug                = flag.Bool("debug", false, "Enable debug output")
	apiAddr              = flag.String("api-addr", "http://localhost:8080/", "qBittorrent API address")
	port                 = flag.Uint("port", 9091, "Transmission RPC port")
	accurateTrackerStats = flag.Bool("accurate-tracker-stats", false, "Fast (and less precise) trackerStats response")
	disableKeepAlive     = flag.Bool("disable-keep-alive", false, "Disable HTTP Keep-Alive in requests (may be necessary for older qBittorrent versions)")
)

func init() {
	flag.BoolVar(verbose, "v", false, "")
	flag.BoolVar(debug, "d", false, "")
	flag.StringVar(apiAddr, "r", "http://localhost:8080/", "")
	flag.UintVar(port, "p", 9091, "")
	flag.Parse()
}

var deprecatedFields = []string{
	"announceUrl",
	"announceResponse",
	"seeders",
	"leechers",
	"downloadLimitMode",
	"uploadLimitMode",
	"nextAnnounceTime",
}

var qBTConn qBT.Connection

func IsFieldDeprecated(field string) bool {
	for _, value := range deprecatedFields {
		if value == field {
			return true
		}
	}
	return false
}

func parseIDsArgument(args *json.RawMessage) []int {
	allIds := parseIDsField(args)
	filtered := make([]int, 0)
	for _, id := range allIds {
		if qBTConn.GetHashForId(id) != "" {
			filtered = append(filtered, id)
		}
	}
	return filtered
}

func parseIDsField(args *json.RawMessage) []int {
	if args == nil {
		log.Debug("No IDs provided")
		result := make([]int, qBTConn.GetHashNum())
		for i := 0; i < qBTConn.GetHashNum(); i++ {
			result[i] = i + 1
		}
		return result
	}

	var ids interface{}
	err := json.Unmarshal(*args, &ids)
	Check(err)

	switch ids := ids.(type) {
	case float64:
		log.Debug("Query a single ID")
		return []int{int(ids)}
	case []interface{}:
		log.Debug("Query an ID list of length ", len(ids))
		result := make([]int, len(ids))
		for i, value := range ids {
			switch id := value.(type) {
			case float64:
				result[i] = int(id)
			case string:
				var exists bool
				result[i], exists = qBTConn.GetIdOfHash(id)
				if !exists {
					panic("hash not found")
				}
			}
		}
		return result
	case string:
		log.Debug("Query recently-active") // TODO
		result := make([]int, qBTConn.GetHashNum())
		for i := 0; i < qBTConn.GetHashNum(); i++ {
			result[i] = i + 1
		}
		return result
	default:
		log.Error("Unknown action")
		return []int{}
	}
}

func parseActionArgument(args json.RawMessage) []string {
	var req struct {
		Ids json.RawMessage
	}
	err := json.Unmarshal(args, &req)
	Check(err)

	ids := parseIDsArgument(&req.Ids)
	hashes := make([]string, len(ids))
	for i, value := range ids {
		hashes[i] = qBTConn.GetHashForId(value)
	}
	return hashes
}

func MapTorrentList(dst JsonMap, torrentsList []qBT.TorrentsList, id int) {
	var src qBT.TorrentsList
	src = getTorrentById(torrentsList, id)

	for key, value := range transmission.TorrentGetBase {
		dst[key] = value
	}
	dst["hashString"] = src.Hash
	convertedName := EscapeString(src.Name)
	dst["name"] = convertedName
	dst["addedDate"] = src.Added_on
	dst["startDate"] = src.Added_on // TODO
	dst["doneDate"] = src.Completion_on
	dst["recheckProgress"] = src.Progress
	dst["sizeWhenDone"] = src.Size
	dst["totalSize"] = src.Total_size
	dst["downloadDir"] = EscapeString(src.Save_path)
	dst["rateDownload"] = src.Dlspeed
	dst["rateUpload"] = src.Upspeed
	dst["uploadRatio"] = src.Ratio
	dst["eta"] = src.Eta
	dst["status"] = qBTStateToTransmissionStatus(src.State)
	dst["error"] = qBTStateToTransmissionError(src.State)
	dst["isStalled"] = qBTStateToTransmissionStalled(src.State)
	dst["percentDone"] = src.Progress
	dst["peersGettingFromUs"] = src.Num_leechs
	dst["peersSendingToUs"] = src.Num_seeds
	dst["leftUntilDone"] = float64(src.Size) * (1 - src.Progress)
	dst["desiredAvailable"] = float64(src.Size) * (1 - src.Progress) // TODO
	dst["haveUnchecked"] = 0                                         // TODO
}

func getTorrentById(torrentsList []qBT.TorrentsList, id int) (src qBT.TorrentsList) {
	for _, value := range torrentsList {
		if value.Hash == qBTConn.GetHashForId(id) {
			src = value
			return
		}
	}
	panic("Could not find id " + strconv.Itoa(id))
}

const TR_STAT_OK = 0
const TR_STATUS_LOCAL_ERROR = 3

func qBTStateToTransmissionError(state string) int {
	if state == "error" {
		return TR_STATUS_LOCAL_ERROR // TR_STAT_LOCAL_ERROR
	} else {
		return TR_STAT_OK // TR_STAT_OK
	}
}

func qBTStateToTransmissionStalled(state string) bool {
	switch state {
	case "stalledDL", "stalledUP":
		return true
	default:
		return false
	}
}

const TR_STATUS_STOPPED = 0
const TR_STATUS_CHECK = 2
const TR_STATUS_DOWNLOAD_WAIT = 3
const TR_STATUS_DOWNLOAD = 4
const TR_STATUS_SEED_WAIT = 5
const TR_STATUS_SEED = 6

func qBTStateToTransmissionStatus(state string) int {
	switch state {
	case "pausedUP", "pausedDL":
		return TR_STATUS_STOPPED // TR_STATUS_STOPPED
	case "checkingUP", "checkingDL":
		return TR_STATUS_CHECK // TR_STATUS_CHECK
	case "queuedDL":
		return TR_STATUS_DOWNLOAD_WAIT // TR_STATUS_DOWNLOAD_WAIT
	case "downloading", "stalledDL":
		return TR_STATUS_DOWNLOAD // TR_STATUS_DOWNLOAD
	case "queuedUP":
		return TR_STATUS_SEED_WAIT // TR_STATUS_SEED_WAIT
	case "uploading", "stalledUP":
		return TR_STATUS_SEED // TR_STATUS_SEED
	case "error":
		return TR_STATUS_STOPPED // TR_STATUS_STOPPED
	default:
		return TR_STATUS_STOPPED // TR_STATUS_STOPPED
	}
}

func MapPieceStates(dst JsonMap, pieces []byte) {
	bits := bitarray.NewSparseBitArray()

	for i, value := range pieces {
		if value == 2 {
			bits.SetBit(uint64(i))
		}
	}

	serialized, _ := bitarray.Marshal(bits)

	dst["pieces"] = base64.StdEncoding.EncodeToString(serialized)
}

func MakePiecesBitArray(total, have int) string {
	if (total < 0) || (have < 0) {
		return "" // Empty array
	}
	arrLen := uint(math.Ceil(float64(total) / 8))
	arr := make([]byte, arrLen)

	fullBytes := uint(math.Floor(float64(have) / 8))
	for i := uint(0); i < fullBytes; i++ {
		arr[i] = math.MaxUint8
	}
	for i := uint(0); i < (uint(have) - fullBytes*8); i++ {
		arr[fullBytes] |= 128 >> i
	}

	return base64.StdEncoding.EncodeToString(arr)
}

func isMn(r rune) bool {
	return unicode.Is(unicode.Mn, r) // Mn: nonspacing marks
}

type escapedString string

func (s escapedString) MarshalJSON() ([]byte, error) {
	return []byte(strconv.QuoteToASCII(string(s))), nil
}

func EscapeString(in string) escapedString {
	return escapedString(in)
}

func MapPropsGeneral(dst JsonMap, propGeneral qBT.PropertiesGeneral) {
	dst["pieceSize"] = propGeneral.Piece_size
	dst["pieceCount"] = propGeneral.Pieces_num
	dst["addedDate"] = propGeneral.Addition_date
	dst["startDate"] = propGeneral.Addition_date // TODO
	dst["comment"] = propGeneral.Comment
	dst["dateCreated"] = propGeneral.Creation_date
	dst["creator"] = propGeneral.Created_by
	dst["doneDate"] = propGeneral.Completion_date
	dst["totalSize"] = propGeneral.Total_size
	dst["haveValid"] = propGeneral.Piece_size * propGeneral.Pieces_have
	dst["downloadedEver"] = propGeneral.Total_downloaded
	dst["uploadedEver"] = propGeneral.Total_uploaded
	dst["peersConnected"] = propGeneral.Peers
	dst["peersFrom"] = struct {
		fromCache    int
		fromDht      int
		fromIncoming int
		fromLdp      int
		fromLtep     int
		fromPex      int
		fromTracker  int
	}{
		fromTracker: propGeneral.Peers,
	}
	dst["corruptEver"] = propGeneral.Total_wasted

	if propGeneral.Up_limit >= 0 {
		dst["uploadLimited"] = true
		dst["uploadLimit"] = propGeneral.Up_limit
	} else {
		dst["uploadLimited"] = false
		dst["uploadLimit"] = 0
	}

	if propGeneral.Dl_limit >= 0 {
		dst["downloadLimited"] = true
		dst["downloadLimit"] = propGeneral.Dl_limit // TODO: Kb/s?
	} else {
		dst["downloadLimited"] = false
		dst["downloadLimit"] = 0
	}

	dst["maxConnectedPeers"] = propGeneral.Nb_connections_limit
	dst["peer-limit"] = propGeneral.Nb_connections_limit // TODO: What's it?
}

func MapPropsPeers(dst JsonMap, hash string) {
	url := qBTConn.MakeRequestURLWithParam("sync/torrentPeers", map[string]string{"hash": hash, "rid": "0"})
	torrents := qBTConn.DoGET(url)

	log.Debug(string(torrents))
	var resp struct {
		//Peers map[string]qBT.PeerInfo
		Peers map[string]qBT.PeerInfo
	}
	err := json.Unmarshal(torrents, &resp)
	Check(err)
	//var trPeers []transmission.PeerInfo
	trPeers := make([]transmission.PeerInfo, 0)

	for _, peer := range resp.Peers {
		clientName := EscapeString(peer.Client)
		country := EscapeString(peer.Country)
		trPeers = append(trPeers, transmission.PeerInfo{
			RateToPeer:   peer.Up_speed,
			RateToClient: peer.Dl_speed,
			ClientName:   clientName,
			FlagStr:      peer.Flags,
			Country:      country,
			Address:      peer.IP,
			Progress:     peer.Progress,
			Port:         peer.Port,
		})
	}

	dst["peers"] = trPeers
}

func MapPropsTrackers(dst JsonMap, trackers []qBT.PropertiesTrackers) {
	trackersList := make([]JsonMap, len(trackers))

	for i, value := range trackers {
		h := fnv.New32a()
		h.Write([]byte(value.Url))
		id := h.Sum32()
		trackersList[i] = make(JsonMap)
		trackersList[i]["announce"] = value.Url
		trackersList[i]["id"] = id
		trackersList[i]["scrape"] = value.Url
		trackersList[i]["tier"] = 0 // TODO
	}

	dst["trackers"] = trackersList
}

func MapPropsTrackerStats(dst JsonMap, trackers []qBT.PropertiesTrackers, torrentList qBT.TorrentsList) {
	trackerStats := make([]JsonMap, len(trackers))

	for i, value := range trackers {
		h := fnv.New32a()
		h.Write([]byte(value.Url))
		id := h.Sum32()

		trackerStats[i] = make(JsonMap)
		for key, value := range transmission.TrackerStatsTemplate {
			trackerStats[i][key] = value
		}
		trackerStats[i]["announce"] = value.Url
		trackerStats[i]["host"] = value.Url
		trackerStats[i]["leecherCount"] = torrentList.Num_incomplete
		trackerStats[i]["seederCount"] = torrentList.Num_complete
		trackerStats[i]["downloadCount"] = torrentList.Num_complete                                      // TODO: Find a more accurate source
		trackerStats[i]["lastAnnouncePeerCount"] = torrentList.Num_complete + torrentList.Num_incomplete // TODO: Is it correct?
		trackerStats[i]["lastAnnounceResult"] = decodeTrackerStatus(value.Status)
		trackerStats[i]["lastAnnounceSucceeded"] = value.Status == 2
		trackerStats[i]["hasAnnounced"] = value.Status == 2
		trackerStats[i]["id"] = id
		trackerStats[i]["scrape"] = ""
		trackerStats[i]["tier"] = 0 // TODO
	}

	dst["trackerStats"] = trackerStats
}

func decodeTrackerStatus(status int) string {
	switch status {
	case 0:
		return "Tracker is disabled"
	case 1:
		return "Tracker has not been contacted yet"
	case 2:
		return "Tracker has been contacted and is working"
	case 3:
		return "Tracker is updating"
	case 4:
		return "Tracker has been contacted, but it is not working (or doesn't send proper replies)"
	default:
		return "Unknown status"
	}
}

func MapPropsTrackerStatsFake(dst JsonMap, torrentList qBT.TorrentsList) {
	trackerStats := JsonMap{}

	for key, value := range transmission.TrackerStatsTemplate {
		trackerStats[key] = value
	}
	trackerStats["announce"] = ""
	trackerStats["host"] = ""
	trackerStats["leecherCount"] = torrentList.Num_incomplete
	trackerStats["seederCount"] = torrentList.Num_complete
	trackerStats["downloadCount"] = torrentList.Num_complete                                      // TODO: Find a more accurate source
	trackerStats["lastAnnouncePeerCount"] = torrentList.Num_complete + torrentList.Num_incomplete // TODO: Is it correct?
	trackerStats["lastAnnounceResult"] = "OK"
	trackerStats["lastAnnounceSucceeded"] = true
	trackerStats["hasAnnounced"] = true
	trackerStats["id"] = 0
	trackerStats["scrape"] = ""
	trackerStats["tier"] = 0 // TODO

	dst["trackerStats"] = []JsonMap{trackerStats}
}

func MapPropsFiles(dst JsonMap, filesInfo []qBT.PropertiesFiles) {
	fileNum := len(filesInfo)
	files := make([]JsonMap, fileNum)
	fileStats := make([]JsonMap, fileNum)
	priorities := make([]int, fileNum)
	wanted := make([]int, fileNum)
	for i, value := range filesInfo {
		files[i] = make(JsonMap)
		fileStats[i] = make(JsonMap)

		files[i]["bytesCompleted"] = float64(value.Size) * value.Progress
		files[i]["length"] = value.Size
		convertedName := EscapeString(value.Name)
		files[i]["name"] = convertedName

		fileStats[i]["bytesCompleted"] = float64(value.Size) * value.Progress
		if value.Priority == 0 {
			fileStats[i]["wanted"] = false
			wanted[i] = 0
		} else {
			fileStats[i]["wanted"] = true
			wanted[i] = 1
		}
		fileStats[i]["priority"] = 0 // TODO
		priorities[i] = 0            // TODO
	}

	dst["files"] = files
	dst["fileStats"] = fileStats
	dst["priorities"] = priorities
	dst["wanted"] = wanted
}

var propsCache = Cache{}
var trackersCache = Cache{}
var trackerStatsCache = Cache{}

func (m JsonMap) addAll(source JsonMap)  {
	for key, value := range *source {
		m[key] = value
	}
}

func TorrentGet(args json.RawMessage) (JsonMap, string) {
	var req transmission.GetRequest
	err := json.Unmarshal(args, &req)
	Check(err)

	torrentList := qBTConn.GetTorrentList(nil)

	ids := parseIDsArgument(req.Ids)
	fields := req.Fields
	filesNeeded := false
	trackersNeeded := false
	trackerStatsNeeded := false
	peersNeeded := false
	propsGeneralNeeded := false
	piecesNeeded := false
	for _, field := range fields {
		additionalRequestsNeeded := true
		switch field {
		case "files", "fileStats", "priorities", "wanted":
			filesNeeded = true
		case "trackers":
			trackersNeeded = true
		case "trackerStats":
			trackerStatsNeeded = true
			if !*accurateTrackerStats {
				additionalRequestsNeeded = false
			}
		case "peers":
			peersNeeded = true
		case "pieceSize", "pieceCount",
			"comment", "dateCreated", "creator",
			"haveValid", "downloadedEver",
			"uploadedEver", "peersConnected", "peersFrom",
			"corruptEver", "uploadLimited", "uploadLimit", "downloadLimited",
			"downloadLimit", "maxConnectedPeers", "peer-limit":
			propsGeneralNeeded = true
		case "pieces":
			piecesNeeded = true
		default:
			additionalRequestsNeeded = false
		}
		if additionalRequestsNeeded && req.Ids == nil {
			log.Info("Field which caused a full torrent scan (slow op!): " + field)
		}
	}

	resultList := make([]JsonMap, len(ids))
	for i, id := range ids {
		translated := make(JsonMap)

		MapTorrentList(translated, torrentList, id) // TODO: Make it conditional too

		if propsGeneralNeeded {
			log.Debug("Props required")
			propsCache.GetOrFill(translated, func(dest JsonMap) {
				propGeneral := qBTConn.GetPropsGeneral(id)
				MapPropsGeneral(dest, propGeneral)
			})
			if ok, values := propsCache.IsStillValid(15 * time.Second); ok {
				translated.addAll(*values)
			} else {
				newValues := make(JsonMap)
				propGeneral := qBTConn.GetPropsGeneral(id)
				MapPropsGeneral(newValues, propGeneral)
				translated.addAll(newValues)
				propsCache.Fill(&newValues)
			}
		}
		if trackersNeeded || (trackerStatsNeeded && *accurateTrackerStats) {
			log.Debug("Trackers required")
			trackers := qBTConn.GetPropsTrackers(id)
			MapPropsTrackers(translated, trackers)
			MapPropsTrackerStats(translated, trackers, getTorrentById(torrentList, id))
		} else if trackerStatsNeeded {
			MapPropsTrackerStatsFake(translated, getTorrentById(torrentList, id))
		}
		if piecesNeeded {
			log.Debug("Pieces required")
			pieces := qBTConn.GetPiecesStates(id)
			MapPieceStates(translated, pieces)
		}
		if filesNeeded {
			log.Debug("Files required")
			files := qBTConn.GetPropsFiles(id)
			MapPropsFiles(translated, files)
		}
		if peersNeeded {
			log.Debug("Peers required")
			MapPropsPeers(translated, qBTConn.GetHashForId(id))
		}

		translated["id"] = id
		translated["queuePosition"] = i + 1
		// TODO: Check it once
		for _, field := range fields {
			if _, ok := translated[field]; !ok {
				if !IsFieldDeprecated(field) {
					log.Error("Unsupported field: ", field)
					panic("Unsupported field: " + field)
				}
			}
		}
		for translatedField := range translated {
			if !Any(fields, translatedField) {
				// Remove unneeded fields
				delete(translated, translatedField)
			}
		}
		resultList[i] = translated
	}
	return JsonMap{"torrents": resultList}, "success"
}

func qBTEncryptionToTR(enc int) (res string) {
	switch enc {
	case 0:
		return "preferred"
	case 1:
		return "required"
	default:
		return "tolerated"
	}
}

func SessionGet() (JsonMap, string) {
	session := make(JsonMap)
	for key, value := range transmission.SessionGetBase {
		session[key] = value
	}

	prefs := qBTConn.GetPreferences()
	session["download-dir"] = prefs.Save_path
	session["speed-limit-down"] = prefs.Dl_limit / 1024
	session["speed-limit-up"] = prefs.Up_limit / 1024
	if prefs.Dl_limit == -1 {
		session["speed-limit-down-enabled"] = false
	} else {
		session["speed-limit-down-enabled"] = true
	}

	if prefs.Up_limit == -1 {
		session["speed-limit-up-enabled"] = false
	} else {
		session["speed-limit-up-enabled"] = true
	}

	session["peer-limit-global"] = prefs.Max_connec
	session["peer-limit-per-torrent"] = prefs.Max_connec_per_torrent
	session["peer-port"] = prefs.Listen_port
	session["seedRatioLimit"] = prefs.Max_ratio
	session["seedRatioLimited"] = prefs.Max_ratio_enabled
	session["peer-port-random-on-start"] = prefs.Random_port
	session["port-forwarding-enabled"] = prefs.Upnp
	session["utp-enabled"] = prefs.Enable_utp
	session["dht-enabled"] = prefs.Dht
	session["incomplete-dir"] = prefs.Temp_path
	session["incomplete-dir-enabled"] = prefs.Temp_path_enabled
	session["lpd-enabled"] = prefs.Lsd
	session["pex-enabled"] = prefs.Pex
	session["encryption"] = qBTEncryptionToTR(prefs.Encryption)
	session["download-queue-size"] = prefs.Max_active_downloads
	session["seed-queue-size"] = prefs.Max_active_uploads
	session["download-queue-enabled"] = prefs.Queueing_enabled
	session["seed-queue-enabled"] = prefs.Queueing_enabled
	session["download-dir"] = prefs.Save_path

	version := qBTConn.GetVersion()
	session["version"] = "2.84 (really qBT " + string(version) + ")"
	return session, "success"
}

func FreeSpace(args json.RawMessage) (JsonMap, string) {
	var req JsonMap
	err := json.Unmarshal(args, &req)
	Check(err)

	var path string
	switch v := req["path"].(type) {
	case string:
		path = v
	}
	size := uint64(100 * (1 << 30))
	if path != "" {
		var stat syscall.Statfs_t
		syscall.Statfs(path, &stat)
		size = stat.Bavail * uint64(stat.Bsize)
	}

	log.Debug("Free space of ", path, ": ", size)

	return JsonMap{
		"path":       path,
		"size-bytes": size, // 100 GB
	}, "success"
}

func SessionStats() (JsonMap, string) {
	session := make(JsonMap)
	for key, value := range transmission.SessionStatsTemplate {
		session[key] = value
	}

	torrentList := qBTConn.GetTorrentList(nil)

	ids := parseIDsArgument(nil)

	paused := 0
	active := 0
	all := 0
	timeElapsed := 0

	for _, id := range ids {
		torrent := getTorrentById(torrentList, id)

		if qBTStateToTransmissionStatus(torrent.State) == TR_STATUS_STOPPED {
			paused++
		} else {
			active++
		}
		all++

	}

	info := qBTConn.GetTransferInfo()
	session["activeTorrentCount"] = active
	session["pausedTorrentCount"] = paused
	session["torrentCount"] = all
	session["downloadSpeed"] = info.Dl_info_speed
	session["uploadSpeed"] = info.Up_info_speed
	session["current-stats"].(map[string]int64)["downloadedBytes"] = info.Dl_info_data
	session["current-stats"].(map[string]int64)["uploadedBytes"] = info.Up_info_data
	session["current-stats"].(map[string]int64)["secondsActive"] = int64(timeElapsed)
	session["cumulative-stats"] = session["current-stats"]
	return session, "success"
}

func TorrentPause(args json.RawMessage) (JsonMap, string) {
	hashes := parseActionArgument(args)
	for _, hash := range hashes {
		log.Debug("Stopping torrent with hash ", hash)

		qBTConn.PostForm(qBTConn.MakeRequestURL("torrents/pause"),
			url.Values{"hashes": {hash}})
	}
	return JsonMap{}, "success"
}

func TorrentResume(args json.RawMessage) (JsonMap, string) {
	hashes := parseActionArgument(args)
	for _, hash := range hashes {
		log.Debug("Starting torrent with hash ", hash)

		qBTConn.PostForm(qBTConn.MakeRequestURL("torrents/resume"),
			url.Values{"hashes": {hash}})
	}
	return JsonMap{}, "success"
}

func TorrentRecheck(args json.RawMessage) (JsonMap, string) {
	hashes := parseActionArgument(args)
	for _, hash := range hashes {
		log.Debug("Verifying torrent with hash ", hash)

		qBTConn.PostForm(qBTConn.MakeRequestURL("torrents/recheck"),
			url.Values{"hashes": {hash}})
	}
	return JsonMap{}, "success"
}

func TorrentDelete(args json.RawMessage) (JsonMap, string) {
	var req struct {
		Ids               json.RawMessage
		Delete_local_data interface{} `json:"delete-local-data"`
	}
	err := json.Unmarshal(args, &req)
	Check(err)

	ids := parseIDsArgument(&req.Ids)
	hashes := make([]string, len(ids))
	for i, value := range ids {
		hashes[i] = qBTConn.GetHashForId(value)

		torrents := qBTConn.GetTorrentList(nil)
		for _, torrent := range torrents {
			if torrent.Hash == hashes[i] {
				log.Warn("Going to remove torrent named ", torrent.Name, " with hash ", torrent.Hash)
			}
		}

		//qBTConn.HashIds[value-1] = ""
	}

	joinedHashes := strings.Join(hashes, "|")

	var deleteFiles bool // TODO: Move to a function
	switch val := req.Delete_local_data.(type) {
	case bool:
		deleteFiles = val
	case float64:
		deleteFiles = (val != 0)
	}

	params := map[string]string{"hashes": joinedHashes}

	if deleteFiles {
		log.Info("Going to remove torrent with files: ", joinedHashes)
		params["deleteFiles"] = "true"
	} else {
		log.Info("Going to remove torrent: ", joinedHashes)
		params["deleteFiles"] = "false"
	}
	url := qBTConn.MakeRequestURLWithParam("torrents/delete", params)
	qBTConn.DoGET(url)

	return JsonMap{}, "success"
}

func UploadTorrent(metainfo *[]byte, urls *string, destDir *string, paused bool) {
	var buffer bytes.Buffer
	mime := multipart.NewWriter(&buffer)

	if metainfo != nil {
		mimeWriter, err := mime.CreateFormFile("torrents", "example.torrent")
		Check(err)
		mimeWriter.Write(*metainfo)
	}

	if urls != nil {
		urlsWriter, err := mime.CreateFormField("urls")
		Check(err)
		urlsWriter.Write([]byte(*urls))
	}

	if destDir != nil {
		destDirWriter, err := mime.CreateFormField("savepath")
		Check(err)
		destDirWriter.Write([]byte(*destDir))
	}
	pausedWriter, err := mime.CreateFormField("paused")
	Check(err)
	if paused {
		pausedWriter.Write([]byte("true"))
	} else {
		pausedWriter.Write([]byte("false"))
	}
	mime.CreateFormField("cookie")
	mime.CreateFormField("label")

	mime.Close()

	qBTConn.DoPOST(qBTConn.MakeRequestURL("torrents/add"), mime.FormDataContentType(), &buffer)
	log.Debug("Torrent uploaded")
}

func ParseMagnetLink(link string) (newHash, newName string) {
	path := strings.TrimPrefix(link, "magnet:?")
	params, err := url.ParseQuery(path)
	Check(err)
	log.WithFields(log.Fields{
		"params": params,
	}).Debug("Params decoded")
	trimmed := strings.TrimPrefix(params["xt"][0], "urn:btih:")
	newHash = strings.ToLower(trimmed)
	name, nameProvided := params["dn"]
	if nameProvided {
		newName = name[0]
	} else {
		newName = "Torrent name"
	}
	return
}

func ParseMetainfo(metainfo []byte) (newHash, newName string) {
	var parsedMetaInfo MetaInfo
	parsedMetaInfo.ReadTorrentMetaInfoFile(bytes.NewBuffer(metainfo))

	log.WithFields(log.Fields{
		"len":  len(metainfo),
		"sha1": fmt.Sprintf("%x\n", sha1.Sum(metainfo)),
	}).Debug("Decoded metainfo")

	newHash = fmt.Sprintf("%x", parsedMetaInfo.InfoHash)
	newName = parsedMetaInfo.Info.Name
	return
}

func TorrentAdd(args json.RawMessage) (JsonMap, string) {
	var req transmission.TorrentAddRequest
	err := json.Unmarshal(args, &req)
	Check(err)

	qBTConn.GetTorrentList(nil)

	var newHash string
	var newName string

	paused := false
	if req.Paused != nil {
		if value, ok := (*req.Paused).(float64); ok {
			// Workaround: Transmission Remote GUI uses a number instead of a boolean
			log.Debug("Apply Transmission Remote GUI workaround")
			paused = value != 0
		}
		if value, ok := (*req.Paused).(bool); ok {
			paused = value
		}
	}

	if req.Metainfo != nil {
		log.Debug("Upload torrent from metainfo")
		metainfo, err := base64.StdEncoding.DecodeString(*req.Metainfo)
		Check(err)
		UploadTorrent(&metainfo, nil, req.Download_dir, paused)
	} else if req.Filename != nil {
		path := *req.Filename
		if strings.HasPrefix(path, "magnet:?") {
			newHash, newName = ParseMagnetLink(path)

			UploadTorrent(nil, &path, req.Download_dir, paused)
		} else if strings.HasPrefix(path, "http") {
			metainfo := DoGetWithCookies(path, req.Cookies)

			newHash, newName = ParseMetainfo(metainfo)
			UploadTorrent(&metainfo, nil, nil, paused)
		}
	}

	log.WithFields(log.Fields{
		"hash": newHash,
		"name": newName,
	}).Debug("Attempting to add torrent")

	if newId, exists := qBTConn.GetIdOfHash(newHash); exists {
		return JsonMap{
			"torrent-duplicate": JsonMap{
				"id":         newId,
				"name":       newName,
				"hashString": newHash,
			},
		}, "success"
	}

	exists := false
	newId := 0
	for retries := 0; retries < 100; retries++ {
		time.Sleep(50 * time.Millisecond)
		qBTConn.GetTorrentList(nil)
		newId, exists = qBTConn.GetIdOfHash(newHash)
		if exists {
			log.Debug("Found ID ", newId)
			break
		}

		log.Debug("Nothing was found, waiting...")
	}

	if !exists {
		return JsonMap{}, "Torrent-add timeout"
	}

	log.WithFields(log.Fields{
		"hash": newHash,
		"id":   newId,
		"name": newName,
	}).Debug("New torrent")

	return JsonMap{
		"torrent-added": JsonMap{
			"id":         newId,
			"name":       newName,
			"hashString": newHash,
		},
	}, "success"
}

func TorrentSet(args json.RawMessage) (JsonMap, string) {
	var req struct {
		Ids            *json.RawMessage
		Files_wanted   *[]int `json:"files-wanted"`
		Files_unwanted *[]int `json:"files-unwanted"`
	}
	err := json.Unmarshal(args, &req)
	Check(err)

	if req.Files_wanted != nil || req.Files_unwanted != nil {
		ids := parseIDsField(req.Ids)
		if len(ids) != 1 {
			log.Error("Unsupported torrent-set request")
			return JsonMap{}, "Unsupported torrent-set request"
		}
		id := ids[0]

		newFilesPriorities := make(map[int]int)
		if req.Files_wanted != nil {
			wanted := *req.Files_wanted
			for _, fileId := range wanted {
				newFilesPriorities[fileId] = 1 // Normal priority
			}
		}
		if req.Files_unwanted != nil {
			unwanted := *req.Files_unwanted
			for _, fileId := range unwanted {
				newFilesPriorities[fileId] = 0 // Do not download
			}
		}
		log.WithFields(log.Fields{
			"priorities": newFilesPriorities,
		}).Debug("New files priorities")

		for fileId, priority := range newFilesPriorities {
			params := url.Values{
				"hash":     {qBTConn.GetHashForId(id)},
				"id":       {strconv.Itoa(fileId)},
				"priority": {strconv.Itoa(priority)},
			}
			qBTConn.PostForm(qBTConn.MakeRequestURL("torrents/filePrio"), params)
		}
	}

	return JsonMap{}, "success" // TODO
}

func TorrentSetLocation(args json.RawMessage) (JsonMap, string) {
	var req struct {
		Ids      *json.RawMessage
		Location *string     `json: "location"`
		Move     interface{} `json:"move"`
	}
	err := json.Unmarshal(args, &req)
	Check(err)

	log.Debug("New location: ", *req.Location)
	if req.Location != nil {
		ids := parseIDsField(req.Ids)
		if len(ids) != 1 {
			log.Error("Unsupported torrent-set-location request")
			return JsonMap{}, "Unsupported torrent-set-location request"
		}
		id := ids[0]

		/*var move bool // TODO: Move to a function
		switch val := req.Move.(type) {
		case bool:
			move = val
		case float64:
			move = (val != 0)
		}*/

		params := url.Values{
			"hashes":   {qBTConn.GetHashForId(id)},
			"location": {*req.Location},
		}
		qBTConn.PostForm(qBTConn.MakeRequestURL("torrents/setLocation"), params)
	}

	return JsonMap{}, "success" // TODO
}

func handler(w http.ResponseWriter, r *http.Request) {
	var req transmission.RPCRequest
	reqBody, err := ioutil.ReadAll(r.Body)
	log.Debug("Got request ", string(reqBody))
	err = json.Unmarshal(reqBody, &req)
	Check(err)

	if !qBTConn.Auth.LoggedIn {
		var authOK = false
		username, password, present := r.BasicAuth()
		if present {
			authOK = qBTConn.Login(username, password)
		} else {
			authOK = qBTConn.Login("", "")
		}
		if !authOK {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}

	var resp JsonMap
	var result string
	switch req.Method {
	case "session-get":
		resp, result = SessionGet()
	case "free-space":
		resp, result = FreeSpace(req.Arguments)
	case "torrent-get":
		resp, result = TorrentGet(req.Arguments)
	case "session-stats":
		resp, result = SessionStats()
	case "torrent-stop":
		resp, result = TorrentPause(req.Arguments)
	case "torrent-start":
		resp, result = TorrentResume(req.Arguments)
	case "torrent-start-now":
		resp, result = TorrentResume(req.Arguments)
	case "torrent-verify":
		resp, result = TorrentRecheck(req.Arguments)
	case "torrent-remove":
		resp, result = TorrentDelete(req.Arguments)
	case "torrent-add":
		resp, result = TorrentAdd(req.Arguments)
	case "torrent-set":
		resp, result = TorrentSet(req.Arguments)
	case "torrent-set-location":
		resp, result = TorrentSetLocation(req.Arguments)
	default:
		log.Error("Unknown method: ", req.Method)
	}
	response := JsonMap{
		"result":    result,
		"arguments": resp,
	}
	if req.Tag != nil {
		response["tag"] = req.Tag
	}
	respBody, err := json.Marshal(response)
	Check(err)
	log.Debug("respBody: ", string(respBody))
	w.Header().Set("Content-Length", strconv.Itoa(len(respBody)))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(respBody)
}

func main() {
	switch {
	case *debug:
		log.SetLevel(log.DebugLevel)
	case *verbose:
		log.SetLevel(log.InfoLevel)
	default:
		log.SetLevel(log.WarnLevel)
	}
	qBTConn.Init(*apiAddr)
	if *disableKeepAlive {
		log.Info("Disabled HTTP keep-alive")
		qBTConn.Tr = &http.Transport{
			DisableKeepAlives: true,
		}
		qBTConn.Client = &http.Client{Transport: qBTConn.Tr}
	} else {
		qBTConn.Tr = &http.Transport{}
		qBTConn.Client = &http.Client{}
	}

	http.HandleFunc("/transmission/rpc", handler)
	http.HandleFunc("/rpc", handler)
	http.Handle("/", http.FileServer(http.Dir("web/")))
	err := http.ListenAndServe(fmt.Sprintf(":%d", *port), nil)
	Check(err)
}
