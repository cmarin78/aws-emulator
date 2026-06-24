// Package s3 emula el subconjunto más usado de la API de Amazon S3:
// buckets y objetos, con el protocolo Query/XML real (no JSON, a
// diferencia de azure-emulator/gcp-emulator que devuelven JSON simple en
// sus servicios de blob storage) — los SDKs de S3 parsean XML real y
// muchos clientes (s3cmd, rclone, mc) no aceptan otra cosa.
//
// Persistencia: tres buckets de BoltDB —
//
//	s3.buckets     bucket name -> Bucket (metadata)
//	s3.objects     "{bucket}/{key}" -> ObjectMeta (metadata)
//	s3.objectdata  "{bucket}/{key}" -> []byte (contenido crudo del objeto)
//
// El contenido se separa de la metadata (igual que ministack separa los
// blobs de código Lambda) para no deserializar el body completo del
// objeto cada vez que solo se necesita su metadata (ETag, tamaño) en un
// ListObjectsV2.
//
// No implementado en Fase 1 (ver ROADMAP.md): multipart upload, ACLs,
// versionado, lifecycle rules, autenticación SigV4 real (las requests no
// se validan, solo se enrutan).
package s3

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cesarmarin/aws-emulator/internal/server"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

const (
	bucketsBucket = "s3.buckets"
	objectsBucket = "s3.objects"
	dataBucket    = "s3.objectdata"
)

// Service agrupa el estado necesario para atender requests de S3.
type Service struct {
	db *storage.DB
}

// New crea el servicio S3.
func New(db *storage.DB) *Service {
	return &Service{db: db}
}

// Bucket es la metadata persistida de un bucket.
type Bucket struct {
	Name         string    `json:"name"`
	CreationDate time.Time `json:"creationDate"`
}

// ObjectMeta es la metadata persistida de un objeto (sin su contenido).
type ObjectMeta struct {
	Bucket       string    `json:"bucket"`
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	ETag         string    `json:"etag"`
	ContentType  string    `json:"contentType"`
	LastModified time.Time `json:"lastModified"`
}

func objectKey(bucket, key string) string {
	return bucket + "/" + key
}

// bucketAndKey extrae bucket/key de una request, soportando tanto
// path-style (PUT /{bucket}/{key}) como virtual-hosted-style
// ({bucket}.s3.amazonaws.com/{key}) — boto3 usa virtual-hosted por
// default desde hace años, así que ambos shapes deben funcionar.
func bucketAndKey(r *http.Request) (bucket, key string) {
	host := r.Host
	if idx := strings.Index(host, ".s3."); idx > 0 {
		bucket = host[:idx]
		key = strings.TrimPrefix(r.URL.Path, "/")
		return bucket, key
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(path, "/", 2)
	bucket = parts[0]
	if len(parts) == 2 {
		key = parts[1]
	}
	return bucket, key
}

// ServeHTTP despacha por método + presencia de bucket/key. Es el único
// punto de entrada registrado en server.Server para el nombre "s3".
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bucket, key := bucketAndKey(r)

	switch {
	case bucket == "" && r.Method == http.MethodGet:
		s.listBuckets(w)
	case bucket != "" && key == "" && r.Method == http.MethodPut:
		s.createBucket(w, bucket)
	case bucket != "" && key == "" && r.Method == http.MethodDelete:
		s.deleteBucket(w, bucket)
	case bucket != "" && key == "" && r.Method == http.MethodGet:
		s.listObjects(w, r, bucket)
	case bucket != "" && key != "" && r.Method == http.MethodPut:
		s.putObject(w, r, bucket, key)
	case bucket != "" && key != "" && r.Method == http.MethodGet:
		s.getObject(w, bucket, key)
	case bucket != "" && key != "" && r.Method == http.MethodHead:
		s.headObject(w, bucket, key)
	case bucket != "" && key != "" && r.Method == http.MethodDelete:
		s.deleteObject(w, bucket, key)
	default:
		server.WriteXMLError(w, http.StatusMethodNotAllowed, "MethodNotAllowed",
			"el emulador no soporta esta combinación de método/ruta para S3")
	}
}

// --- buckets ---

type listAllMyBucketsResult struct {
	XMLName xml.Name        `xml:"ListAllMyBucketsResult"`
	Owner   owner           `xml:"Owner"`
	Buckets bucketsXMLGroup `xml:"Buckets"`
}

type bucketsXMLGroup struct {
	Bucket []bucketXML `xml:"Bucket"`
}

type bucketXML struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

func (s *Service) listBuckets(w http.ResponseWriter) {
	var out []bucketXML
	_ = s.db.List(bucketsBucket, "", func(_ string, raw []byte) error {
		var b Bucket
		if err := json.Unmarshal(raw, &b); err == nil {
			out = append(out, bucketXML{Name: b.Name, CreationDate: b.CreationDate.UTC().Format(time.RFC3339)})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	server.WriteXML(w, http.StatusOK, listAllMyBucketsResult{
		Owner:   owner{ID: "aws-emulator", DisplayName: "aws-emulator"},
		Buckets: bucketsXMLGroup{Bucket: out},
	})
}

func (s *Service) createBucket(w http.ResponseWriter, bucket string) {
	b := Bucket{Name: bucket, CreationDate: time.Now().UTC()}
	if found, _ := s.db.Get(bucketsBucket, bucket, &Bucket{}); found {
		// AWS real responde 200 OK idempotente si el dueño es el mismo
		// (BucketAlreadyOwnedByYou solo aplica fuera de us-east-1); para un
		// emulador local, tratamos PUT repetido como no-op exitoso.
		server.WriteXML(w, http.StatusOK, nil)
		return
	}
	if err := s.db.Put(bucketsBucket, bucket, b); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.Header().Set("Location", "/"+bucket)
	server.WriteXML(w, http.StatusOK, nil)
}

func (s *Service) deleteBucket(w http.ResponseWriter, bucket string) {
	var hasObjects bool
	_ = s.db.List(objectsBucket, bucket+"/", func(string, []byte) error {
		hasObjects = true
		return nil
	})
	if hasObjects {
		server.WriteXMLError(w, http.StatusConflict, "BucketNotEmpty", "el bucket no está vacío")
		return
	}
	if err := s.db.Delete(bucketsBucket, bucket); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- objetos ---

type listBucketResult struct {
	XMLName     xml.Name     `xml:"ListBucketResult"`
	Name        string       `xml:"Name"`
	Prefix      string       `xml:"Prefix"`
	KeyCount    int          `xml:"KeyCount"`
	MaxKeys     int          `xml:"MaxKeys"`
	IsTruncated bool         `xml:"IsTruncated"`
	Contents    []contentXML `xml:"Contents"`
}

type contentXML struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

func (s *Service) listObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	if found, _ := s.db.Get(bucketsBucket, bucket, &Bucket{}); !found {
		server.WriteXMLError(w, http.StatusNotFound, "NoSuchBucket", "el bucket no existe")
		return
	}
	prefix := r.URL.Query().Get("prefix")

	var contents []contentXML
	_ = s.db.List(objectsBucket, bucket+"/", func(_ string, raw []byte) error {
		var m ObjectMeta
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil
		}
		if prefix != "" && !strings.HasPrefix(m.Key, prefix) {
			return nil
		}
		contents = append(contents, contentXML{
			Key:          m.Key,
			LastModified: m.LastModified.UTC().Format(time.RFC3339),
			ETag:         `"` + m.ETag + `"`,
			Size:         m.Size,
			StorageClass: "STANDARD",
		})
		return nil
	})
	sort.Slice(contents, func(i, j int) bool { return contents[i].Key < contents[j].Key })

	server.WriteXML(w, http.StatusOK, listBucketResult{
		Name:        bucket,
		Prefix:      prefix,
		KeyCount:    len(contents),
		MaxKeys:     1000,
		IsTruncated: false,
		Contents:    contents,
	})
}

func (s *Service) putObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if found, _ := s.db.Get(bucketsBucket, bucket, &Bucket{}); !found {
		server.WriteXMLError(w, http.StatusNotFound, "NoSuchBucket", "el bucket no existe")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		server.WriteXMLError(w, http.StatusBadRequest, "InvalidRequest", "no se pudo leer el body")
		return
	}
	sum := md5.Sum(body)
	etag := hex.EncodeToString(sum[:])
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	meta := ObjectMeta{
		Bucket:       bucket,
		Key:          key,
		Size:         int64(len(body)),
		ETag:         etag,
		ContentType:  contentType,
		LastModified: time.Now().UTC(),
	}
	ok := objectKey(bucket, key)
	if err := s.db.Put(objectsBucket, ok, meta); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if err := s.db.PutRaw(dataBucket, ok, body); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.Header().Set("ETag", `"`+etag+`"`)
	server.WriteXML(w, http.StatusOK, nil)
}

func (s *Service) getObject(w http.ResponseWriter, bucket, key string) {
	ok := objectKey(bucket, key)
	var meta ObjectMeta
	found, err := s.db.Get(objectsBucket, ok, &meta)
	if err != nil || !found {
		server.WriteXMLError(w, http.StatusNotFound, "NoSuchKey", "el objeto no existe")
		return
	}
	data, found, err := s.db.GetRaw(dataBucket, ok)
	if err != nil || !found {
		server.WriteXMLError(w, http.StatusNotFound, "NoSuchKey", "el objeto no existe")
		return
	}
	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("ETag", `"`+meta.ETag+`"`)
	w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Service) headObject(w http.ResponseWriter, bucket, key string) {
	ok := objectKey(bucket, key)
	var meta ObjectMeta
	found, err := s.db.Get(objectsBucket, ok, &meta)
	if err != nil || !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("ETag", `"`+meta.ETag+`"`)
	w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.WriteHeader(http.StatusOK)
}

func (s *Service) deleteObject(w http.ResponseWriter, bucket, key string) {
	ok := objectKey(bucket, key)
	_ = s.db.Delete(objectsBucket, ok)
	_ = s.db.Delete(dataBucket, ok)
	w.WriteHeader(http.StatusNoContent)
}
