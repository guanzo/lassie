package httpserver

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	lassie "github.com/filecoin-project/lassie/pkg/lassie"
	"github.com/filecoin-project/lassie/pkg/retriever"
	"github.com/filecoin-project/lassie/pkg/storage"
	"github.com/filecoin-project/lassie/pkg/types"
	"github.com/ipfs/go-cid"
	"github.com/ipld/go-ipld-prime/datamodel"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"github.com/libp2p/go-libp2p/core/peer"
	servertiming "github.com/mitchellh/go-server-timing"
	"github.com/multiformats/go-multicodec"
)

func ipfsHandler(lassie *lassie.Lassie, cfg HttpServerConfig) func(http.ResponseWriter, *http.Request) {
	return func(res http.ResponseWriter, req *http.Request) {
		statusLogger := newStatusLogger(req.Method, req.URL.Path)
		path := datamodel.ParsePath(req.URL.Path)
		_, path = path.Shift() // remove /ipfs

		// filter out everything but GET requests
		switch req.Method {
		case http.MethodGet:
			break
		default:
			res.Header().Add("Allow", http.MethodGet)
			errorResponse(res, statusLogger, http.StatusMethodNotAllowed, errors.New("Method not allowed"))
			return
		}

		// check if CID path param is missing
		if path.Len() == 0 {
			// not a valid path to hit
			errorResponse(res, statusLogger, http.StatusNotFound, errors.New("Not found"))
			return
		}

		includeDupes, err := checkFormat(req)
		if err != nil {
			errorResponse(res, statusLogger, http.StatusBadRequest, err)
			return
		}

		fileName, err := parseFilename(req)
		if err != nil {
			errorResponse(res, statusLogger, http.StatusBadRequest, err)
			return
		}

		// validate CID path parameter
		var cidSeg datamodel.PathSegment
		cidSeg, path = path.Shift()
		rootCid, err := cid.Parse(cidSeg.String())
		if err != nil {
			errorResponse(res, statusLogger, http.StatusInternalServerError, errors.New("Failed to parse CID path parameter"))
			return
		}

		dagScope, err := parseScope(req)
		if err != nil {
			errorResponse(res, statusLogger, http.StatusBadRequest, err)
			return
		}

		protocols, err := parseProtocols(req)
		if err != nil {
			errorResponse(res, statusLogger, http.StatusBadRequest, err)
			return
		}

		fixedPeers, err := parseProviders(req)
		if err != nil {
			errorResponse(res, statusLogger, http.StatusBadRequest, err)
			return
		}

		// for setting Content-Disposition header based on filename url parameter
		if fileName == "" {
			fileName = fmt.Sprintf("%s.car", rootCid.String())
		}

		retrievalId, err := types.NewRetrievalID()
		if err != nil {
			errorResponse(res, statusLogger, http.StatusInternalServerError, fmt.Errorf("Failed to generate retrieval ID: %w", err))
			return
		}

		// TODO: we should propogate this value throughout logs so
		// that we can correlate specific requests to related logs.
		// For now just using to log the corrolation and return the
		// X-Trace-Id header.
		requestId := req.Header.Get("X-Request-Id")
		if requestId == "" {
			requestId = retrievalId.String()
		} else {
			logger.Debugw("Corrolating provided request ID with retrieval ID", "request_id", requestId, "retrieval_id", retrievalId)
		}

		// bytesWritten will be closed once we've started writing CAR content to
		// the response writer. Once closed, no other content should be written.
		bytesWritten := make(chan struct{}, 1)

		tempStore := storage.NewDeferredStorageCar(cfg.TempDir)
		var carWriter storage.DeferredWriter
		if includeDupes {
			carWriter = storage.NewDuplicateAdderCarForStream(req.Context(), rootCid, path.String(), dagScope, tempStore, res)
		} else {
			carWriter = storage.NewDeferredCarWriterForStream(rootCid, res)
		}
		carStore := storage.NewCachingTempStore(carWriter.BlockWriteOpener(), tempStore)
		defer func() {
			if err := carStore.Close(); err != nil {
				logger.Errorf("error closing temp store: %s", err)
			}
		}()
		var store types.ReadableWritableStorage = carStore

		carWriter.OnPut(func(int) {
			// called once we start writing blocks into the CAR (on the first Put())
			res.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileName))
			res.Header().Set("Accept-Ranges", "none")
			res.Header().Set("Cache-Control", "public, max-age=29030400, immutable")
			res.Header().Set("Content-Type", "application/vnd.ipld.car; version=1")
			// TODO: needs scope and path
			res.Header().Set("Etag", fmt.Sprintf("%s.car", rootCid.String()))
			res.Header().Set("X-Content-Type-Options", "nosniff")
			res.Header().Set("X-Ipfs-Path", "/"+datamodel.ParsePath(req.URL.Path).String())
			// TODO: set X-Ipfs-Roots header when we support root+path
			// see https://github.com/ipfs/kubo/pull/8720
			res.Header().Set("X-Trace-Id", requestId)
			statusLogger.logStatus(200, "OK")
			close(bytesWritten)
		}, true)

		request, err := types.NewRequestForPath(store, rootCid, path.String(), dagScope)

		if err != nil {
			errorResponse(res, statusLogger, http.StatusInternalServerError, fmt.Errorf("Failed to create request: %w", err))
			return
		}
		request.Protocols = protocols
		request.FixedPeers = fixedPeers
		request.RetrievalID = retrievalId
		// setup preload storage for bitswap, the temporary CAR store can set up a
		// separate preload space in its storage
		request.PreloadLinkSystem = cidlink.DefaultLinkSystem()
		preloadStore := carStore.PreloadStore()
		request.PreloadLinkSystem.SetReadStorage(preloadStore)
		request.PreloadLinkSystem.SetWriteStorage(preloadStore)
		request.PreloadLinkSystem.TrustedStorage = true

		// extract block limit from query param as needed
		var blockLimit uint64
		if req.URL.Query().Has("blockLimit") {
			if parsedBlockLimit, err := strconv.ParseUint(req.URL.Query().Get("blockLimit"), 10, 64); err == nil {
				blockLimit = parsedBlockLimit
			}
		}
		if cfg.MaxBlocksPerRequest > 0 || blockLimit > 0 {
			// use the lowest non-zero value for block limit
			if blockLimit == 0 || (cfg.MaxBlocksPerRequest > 0 && blockLimit > cfg.MaxBlocksPerRequest) {
				blockLimit = cfg.MaxBlocksPerRequest
			}
			request.MaxBlocks = blockLimit
		}

		logger.Debugw("fetching CID", "retrievalId", retrievalId, "CID", rootCid.String(), "path", path.String(), "dagScope", dagScope)
		stats, err := lassie.Fetch(req.Context(), request, func(re types.RetrievalEvent) {
			header := servertiming.FromContext(req.Context())
			if header == nil {
				return
			}

			header.Lock()
			if header.Metrics != nil {
				for _, m := range header.Metrics {
					if m.Name == string(re.Phase()) {
						if m.Extra == nil {
							m.Extra = map[string]string{}
						}
						m.Extra[string(re.Code())] = fmt.Sprintf("%d", re.Time().Sub(re.PhaseStartTime()))
						header.Unlock()
						return
					}
				}
			}
			header.Unlock()

			metric := header.NewMetric(string(re.Phase()))
			metric.Duration = re.Time().Sub(re.PhaseStartTime())
		})

		// force all blocks to flush
		if cerr := carWriter.Close(); cerr != nil {
			logger.Infof("error closing car writer: %s", cerr)
		}

		if err != nil {
			select {
			case <-bytesWritten:
				reqConn := req.Context().Value(connContextKey)
				if conn, ok := reqConn.(net.Conn); ok {
					res.(http.Flusher).Flush()
					conn.Write([]byte("0\r\n"))
				}
				return
			default:
			}
			if errors.Is(err, retriever.ErrNoCandidates) {
				errorResponse(res, statusLogger, http.StatusNotFound, errors.New("No candidates found"))
			} else {
				errorResponse(res, statusLogger, http.StatusGatewayTimeout, fmt.Errorf("Failed to fetch CID: %w", err))
			}
			return
		}
		logger.Debugw("successfully fetched CID",
			"retrievalId", retrievalId,
			"CID", rootCid,
			"duration", stats.Duration,
			"bytes", stats.Size,
		)
	}
}

// statusLogger is a logger for logging response statuses for a given request
type statusLogger struct {
	method string
	path   string
}

func newStatusLogger(method string, path string) *statusLogger {
	return &statusLogger{method, path}
}

// logStatus logs the method, path, status code and message
func (l statusLogger) logStatus(statusCode int, message string) {
	logger.Infof("%s\t%s\t%d: %s\n", l.method, l.path, statusCode, message)
}

func checkFormat(req *http.Request) (bool, error) {
	hasAccept := req.Header.Get("Accept") != ""
	// check if Accept header includes application/vnd.ipld.car
	validAccept, includeDupes := parceAccept(req.Header.Get("Accept"))

	// check if format is car
	hasFormat := req.URL.Query().Has("format")
	if hasFormat && req.URL.Query().Get("format") != "car" {
		return false, fmt.Errorf("Requested non-supported format %s", req.URL.Query().Get("format"))
	}

	if hasAccept && !validAccept {
		return false, fmt.Errorf("No acceptable content type")
	}
	// if neither are provided return
	// one of them has to be given with a CAR type since we only return CAR data
	if !validAccept && !hasFormat {
		return false, fmt.Errorf("Neither a valid accept header or format parameter were provided")
	}

	return includeDupes, nil
}

func parceAccept(acceptHeader string) (validAccept bool, includeDupes bool) {
	acceptTypes := strings.Split(acceptHeader, ",")
	validAccept = false
	includeDupes = true
	for _, acceptType := range acceptTypes {
		typeParts := strings.Split(acceptType, ";")
		if typeParts[0] == "*/*" || typeParts[0] == "application/*" || typeParts[0] == "application/vnd.ipld.car" {
			validAccept = true
			if typeParts[0] == "application/vnd.ipld.car" {
                        // parse https://github.com/ipfs/specs/pull/412 car attributes
				for _, nextPart := range typeParts[1:] {
					pair := strings.Split(nextPart, "=")
					if len(pair) == 2 {
						attr := strings.TrimSpace(pair[0])
						value := strings.TrimSpace(pair[1])
						switch attr {
						case "dups":
							switch value {
							case "y":
							case "n":
								includeDupes = false
							default:
								// don't accept un expected values
								validAccept = false
							}
						case "version":
							switch value {
							case "1":
							default:
								// don't accept any version but 1
								validAccept = false
							}
						case "order":
							switch value {
							case "dfs":
							case "unk":
							default:
								// we only do dfs, which also satisfies unk, future extensions are not yet supported
								validAccept = false
							}
						default:
							// ignore others
						}
					}
				}
			}
			// only break if further validation didn't fail
			if validAccept {
				break
			}
		}
	}
	return
}

func parseFilename(req *http.Request) (string, error) {
	// check if provided filename query parameter has .car extension
	if req.URL.Query().Has("filename") {
		filename := req.URL.Query().Get("filename")
		ext := filepath.Ext(filename)
		if ext == "" {
			return "", errors.New("Filename missing extension")
		}
		if ext != ".car" {
			return "", fmt.Errorf("Filename uses non-supported extension %s", ext)
		}
		return filename, nil
	}
	return "", nil
}

func parseProtocols(req *http.Request) ([]multicodec.Code, error) {
	if req.URL.Query().Has("protocols") {
		return types.ParseProtocolsString(req.URL.Query().Get("protocols"))
	}
	return nil, nil
}

func parseScope(req *http.Request) (types.DagScope, error) {
	if req.URL.Query().Has("dag-scope") {
		switch req.URL.Query().Get("dag-scope") {
		case "all":
			return types.DagScopeAll, nil
		case "entity":
			return types.DagScopeEntity, nil
		case "block":
			return types.DagScopeBlock, nil
		default:
			return types.DagScopeAll, errors.New("Invalid dag-scope parameter")
		}
	}
	// check for legacy param name -- to do -- delete once we confirm this isn't used any more
	if req.URL.Query().Has("car-scope") {
		switch req.URL.Query().Get("car-scope") {
		case "all":
			return types.DagScopeAll, nil
		case "file":
			return types.DagScopeEntity, nil
		case "block":
			return types.DagScopeBlock, nil
		default:
			return types.DagScopeAll, errors.New("Invalid car-scope parameter")
		}
	}
	return types.DagScopeAll, nil
}

func parseProviders(req *http.Request) ([]peer.AddrInfo, error) {
	if req.URL.Query().Has("providers") {
		fixedPeers, err := types.ParseProviderStrings(req.URL.Query().Get("providers"))
		if err != nil {
			return nil, errors.New("Invalid providers parameter")
		}
		return fixedPeers, nil
	}
	return nil, nil
}

func errorResponse(res http.ResponseWriter, statusLogger *statusLogger, code int, err error) {
	statusLogger.logStatus(code, err.Error())
	http.Error(res, err.Error(), code)
}
