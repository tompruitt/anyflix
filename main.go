package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/types/infohash"
)

const cmdTmpl = `mpv
{{ .StreamURL }}
{{ range .Subtitles }}
--sub-file={{ .URL }}
{{ end }}
`

func main() {
	slog.SetLogLoggerLevel(slog.LevelDebug)

	config := torrent.NewDefaultClientConfig()
	config.DataDir = "/home/user/.cache/anyflix"

	err := os.MkdirAll(config.DataDir, os.ModePerm)
	if err != nil {
		log.Panic(err)
	}

	cl, err := torrent.NewClient(config)
	if err != nil {
		log.Panic(err)
	}
	defer cl.Close()

	subsService := opensubsService{
		"https://opensubtitles-v3.strem.io",
	}
	torrentSearchService := torrentSearchService{
		"https://torrentio.strem.fun",
	}
	metaService := metaService{
		"https://v3-cinemeta.strem.io",
	}
	torrentService := torrentService{
		cl,
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./www/home.html")
	})

	mux.HandleFunc("GET /search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		res, err := metaService.search(q.Get("type"), q.Get("query"))
		if err != nil {
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
		tmpl := template.Must(template.ParseFiles("./www/search.html"))
		err = tmpl.Execute(w, res)
		if err != nil {
			slog.Error("", "err", err)
			return
		}
	})

	mux.HandleFunc("GET /details/{type}/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		kind := r.PathValue("type")

		meta, err := metaService.getMeta(kind, id)
		if err != nil {
			slog.Error("", "err", err)
			return
		}

		tmpl := template.Must(template.ParseFiles("./www/details.html"))

		err = tmpl.Execute(w, struct {
			ID   string
			Meta metaDetailsResponse
		}{
			ID:   id,
			Meta: meta.Meta,
		})
		if err != nil {
			slog.Error("", "err", err)
			return
		}
	})

	mux.HandleFunc("GET /streams/{type}/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		kind := r.PathValue("type")
		tmpl := template.Must(template.ParseFiles("./www/streams.html"))

		streams, err := torrentSearchService.find(kind, id)
		err = tmpl.Execute(w, struct {
			ID      string
			Kind    string
			Streams []StreamDTO
		}{
			ID:      id,
			Kind:    kind,
			Streams: streams.Streams,
		})
		if err != nil {
			slog.Error("", "err", err)
			return
		}
	})

	mux.HandleFunc("GET /watch/{type}/{imdbID}/{infoHash}/{fileIdx}", func(w http.ResponseWriter, r *http.Request) {
		kind := r.PathValue("type")
		infoHash := r.PathValue("infoHash")
		imdbID := r.PathValue("imdbID")

		fileIdx, err := strconv.Atoi(r.PathValue("fileIdx"))
		if err != nil {
			http.Error(w, "invalid fileIdx", http.StatusBadRequest)
			return
		}

		url := fmt.Sprintf(
			"http://localhost:3000/api/torrent/%s/%d/stream",
			infoHash,
			fileIdx,
		)

		hash, err := torrentService.getFileHash(infoHash, fileIdx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		subs, err := subsService.search(kind, imdbID, hash)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		log.Printf("subs: %#+v", subs)

		buf := &bytes.Buffer{}
		err = template.
			Must(template.New("").Parse(cmdTmpl)).
			Execute(buf, struct {
				Subtitles []opensubsSubtitleResponse
				StreamURL string
			}{
				StreamURL: url,
				Subtitles: subs.Subtitles,
			})

		args := strings.Fields(buf.String())
		slog.Info(fmt.Sprint(args))
		cmd := exec.CommandContext(r.Context(), args[0], args[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		err = cmd.Run()
		if err != nil {
			panic(err)
		}
	})

	mux.HandleFunc("GET /api/meta/{type}/search/{query}", metaService.handleSearch)
	mux.HandleFunc("GET /api/streams/{type}/{imdbID}", torrentSearchService.handleSearch)
	mux.HandleFunc("GET /api/torrent/{infoHash}/{fileIdx}/stream", torrentService.handleStreamTorrentFile)
	mux.HandleFunc("GET /api/torrent/{infoHash}/{fileIdx}/hash", torrentService.handleGetFileHash)
	mux.HandleFunc("GET /api/opensubs/{type}/{imdbID}/{fileHash}", subsService.handleSearch)
	//mux.HandleFunc("GET /api/opensubs/{id}", subsService.handleFindSubByID)

	err = http.ListenAndServe(":3000", mux)
	log.Panic(err)
}

type opensubsService struct {
	baseURL string
}

func (h opensubsService) search(kind, imdbID, fileHash string) (opensubsResponse, error) {
	slog.Debug("opensubsService.search", "kind", kind, "imdbID", imdbID, "fileHash", fileHash)

	var subs opensubsResponse
	url := h.baseURL + "/subtitles/" + kind + "/" + imdbID + "videoHash=" + fileHash + ".json"

	resp, err := http.Get(url)
	if err != nil {
		return subs, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return subs, errors.New(resp.Status)
	}

	slog.Info("handleSearch", "method", "GET", "url", url, "status", resp.Status)

	err = json.NewDecoder(resp.Body).Decode(&subs)
	return subs, err
}

func (h opensubsService) handleSearch(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("type")
	imdbID := r.PathValue("imdbID")
	fileHash := r.PathValue("fileHash")

	subs, err := h.search(kind, imdbID, fileHash)
	if err != nil {
		slog.Error("failed to search subtitles",
			"err", err)
		return
	}

	err = json.NewEncoder(w).Encode(subs)
	if err != nil {
		slog.Error("parse subs to json",
			"err", err)
		return
	}
}

type opensubsResponse struct {
	Subtitles []opensubsSubtitleResponse `json:"subtitles"`
}

type opensubsSubtitleResponse struct {
	//ID       int    `json:"id"`
	URL      string `json:"url"`
	Lang     string `json:"lang"`
	Encoding string `json:"SubEncoding"`
}

type metaService struct {
	baseURL string
}

type getMetaResponse struct {
	Meta metaDetailsResponse `json:"meta"`
}

type metaDetailsResponse struct {
	ID          string      `json:"id"`
	Type        string      `json:"type"`
	Name        string      `json:"name"`
	ReleaseInfo string      `json:"releaseInfo"`
	Poster      string      `json:"poster"`
	Videos      []metaVideo `json:"videos"`
}

type metaVideo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Season int    `json:"season"`
	Number int    `json:"number"`
}

func (s metaService) getMeta(kind, id string) (getMetaResponse, error) {
	var res getMetaResponse
	url := s.baseURL + "/meta/" + kind + "/" + id + ".json"

	resp, err := http.Get(url)
	if err != nil {
		slog.Error("search meta",
			"url", url,
			"err", err)
		return res, err
	}
	defer resp.Body.Close()

	err = json.NewDecoder(resp.Body).Decode(&res)
	if err != nil {
		slog.Error("parse meta json",
			"err", err)
		return res, err
	}

	return res, nil
}

func (s metaService) handleGetMeta(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("type")
	id := r.PathValue("id")

	meta, err := s.getMeta(kind, id)
	if err != nil {
		// TODO
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_ = json.NewEncoder(w).Encode(meta)
}

func (s metaService) search(kind string, query string) (searchMetaResponse, error) {
	var res searchMetaResponse
	url := s.baseURL + "/catalog/" + kind + "/top/search=" + query + ".json"

	resp, err := http.Get(url)
	if err != nil {
		slog.Error("search meta",
			"url", url,
			"err", err)
		return res, err
	}
	defer resp.Body.Close()

	err = json.NewDecoder(resp.Body).Decode(&res)
	if err != nil {
		slog.Error("parse meta json",
			"err", err)
		return res, err
	}

	return res, nil
}

func (s metaService) handleSearch(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("type")
	if kind != "movie" && kind != "series" {
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	query := r.PathValue("query")

	res, err := s.search(kind, query)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	err = json.NewEncoder(w).Encode(res)
	if err != nil {
		slog.Error("parse meta to json",
			"err", err)
		return
	}
}

type searchMetaResponse struct {
	Metas []metaResponse `json:"metas"`
}

type metaResponse struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	ReleaseInfo string `json:"release_info"`
	Poster      string `json:"poster"`
}

type torrentService struct {
	client *torrent.Client
}

func (h torrentService) handleStreamTorrentFile(w http.ResponseWriter, r *http.Request) {
	infoHash := r.PathValue("infoHash")
	torrent, _ := h.client.AddTorrentInfoHash(infohash.FromHexString(infoHash))
	<-torrent.GotInfo()

	if len(torrent.Files()) == 0 {
		// TODO
		http.Error(w, "torrent has no files", http.StatusBadRequest)
		return
	}

	fileIdx, err := strconv.Atoi(r.PathValue("fileIdx"))
	if err != nil || fileIdx >= len(torrent.Files()) {
		// TODO
		http.Error(w, "invalid fileIdx", http.StatusBadRequest)
		return
	}

	file := torrent.Files()[fileIdx]

	ranges := strings.SplitN(
		strings.TrimPrefix(r.Header.Get("Range"), "bytes="),
		"-",
		2,
	)

	var start, end int64
	if len(ranges) == 2 {
		start, _ = strconv.ParseInt(ranges[0], 10, 64)
	}
	const chunkSize = 10 * 1024 * 1024
	end = start + chunkSize
	end = min(end, file.Length())

	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "video/mp4") // TODO
	w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, file.Length()))
	w.WriteHeader(http.StatusPartialContent)

	reader := file.NewReader()
	if _, err := reader.Seek(start, io.SeekStart); err != nil {
		slog.Error("failed to seek",
			"start", start,
			"end", end,
			"err", err)
		return
	}

	if _, err := io.CopyN(w, reader, chunkSize); err != nil {
		slog.Error("failed to stream chunk",
			"start", start,
			"end", end,
			"err", err)
		return
	}
}

func (h torrentService) getFileHash(infoHash string, fileIdx int) (string, error) {
	slog.Debug("torrentSevice.getFileHash", "infoHash", infoHash, "fileIdx", fileIdx)

	torrent, _ := h.client.AddTorrentInfoHash(infohash.FromHexString(infoHash))
	<-torrent.GotInfo()

	if len(torrent.Files()) == 0 {
		return "", errors.New("invalid torrent")
	}

	if fileIdx >= len(torrent.Files()) {
		return "", errors.New("invalid fileIdx")
	}

	hash := torrent.Piece(fileIdx).Info().Hash().HexString()
	return hash, nil
}

func (h torrentService) handleGetFileHash(w http.ResponseWriter, r *http.Request) {
	infoHash := r.PathValue("infoHash")

	fileIdx, err := strconv.Atoi(r.PathValue("fileIdx"))
	if err != nil {
		http.Error(w, "invalid fileIdx", http.StatusBadRequest)
		return
	}

	hash, err := h.getFileHash(infoHash, fileIdx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	res := getTorrentFileHashResponse{
		Hash: hash,
	}

	err = json.NewEncoder(w).Encode(res) // TODO
}

type getTorrentFileHashResponse struct {
	Hash string `json:"hash"`
}

type torrentSearchService struct {
	baseURL string
}

func (h torrentSearchService) find(kind, imdbID string) (StreamsDTO, error) {
	var streams StreamsDTO

	resp, err := http.Get(h.baseURL + "/stream/" + kind + "/" + imdbID + ".json")
	if err != nil {
		// TODO log
		return streams, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return streams, errors.New("")
	}

	err = json.NewDecoder(resp.Body).Decode(&streams)
	if err != nil {
		// TODO log
		return streams, err
	}
	return streams, nil
}

func (h torrentSearchService) handleSearch(w http.ResponseWriter, r *http.Request) {
	imdbID := r.PathValue("imdbID") // TODO
	kind := r.PathValue("type")     // TODO

	streams, err := h.find(kind, imdbID)
	if err != nil {
		panic(err)
	}

	err = json.NewEncoder(w).Encode(streams) // TODO
}

type StreamsDTO struct {
	Streams []StreamDTO `json:"streams"`
}

type StreamDTO struct {
	Name     string `json:"name"`
	Title    string `json:"title"`
	InfoHash string `json:"infoHash"`
	FileIdx  int    `json:"fileIdx"`
}

func (h opensubsService) handleFindSubByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	//https://subs-v1.strem.io/osapiv1-
	resp, err := http.Get(h.baseURL + "/" + id)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	_, err = io.Copy(w, resp.Body)
	if err != nil {
		slog.Error("copy subtitle body", "id", id, "err", err)
		return
	}
}
