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
// Fase 2 agrega versionado, tags de objeto y multipart upload (ver
// ROADMAP.md), dispatchados antes del switch principal según el
// query-string sub-resource (?versioning, ?tagging, ?uploads,
// ?uploadId=). Sigue sin implementarse: ACLs, lifecycle rules,
// autenticación SigV4 real (las requests no se validan, solo se
// enrutan).
package s3

import (
	"crypto/md5"
	"crypto/rand"
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
	bucketsBucket     = "s3.buckets"
	objectsBucket     = "s3.objects"
	dataBucket        = "s3.objectdata"
	versioningBucket  = "s3.versioning"
	versionsBucket    = "s3.versions"
	versionDataBucket = "s3.versiondata"
	tagsBucket        = "s3.tags"
	uploadsBucket     = "s3.uploads"
	partsMetaBucket   = "s3.partsmeta"
	partsDataBucket   = "s3.partsdata"
)

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

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
	VersionId    string    `json:"versionId,omitempty"`
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
// Antes del dispatch principal, revisa los sub-recursos del query-string
// (?versioning, ?tagging, ?uploads, ?uploadId=) que en la API real de S3
// se distinguen por query param en vez de por path o método solamente.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bucket, key := bucketAndKey(r)
	q := r.URL.Query()

	switch {
	case bucket != "" && q.Has("versioning") && r.Method == http.MethodPut:
		s.putBucketVersioning(w, r, bucket)
		return
	case bucket != "" && q.Has("versioning") && r.Method == http.MethodGet:
		s.getBucketVersioning(w, bucket)
		return
	case bucket != "" && key != "" && q.Has("tagging") && r.Method == http.MethodPut:
		s.putObjectTagging(w, r, bucket, key)
		return
	case bucket != "" && key != "" && q.Has("tagging") && r.Method == http.MethodGet:
		s.getObjectTagging(w, bucket, key)
		return
	case bucket != "" && key != "" && q.Has("tagging") && r.Method == http.MethodDelete:
		s.deleteObjectTagging(w, bucket, key)
		return
	case bucket != "" && key != "" && q.Has("uploads") && r.Method == http.MethodPost:
		s.createMultipartUpload(w, bucket, key)
		return
	case bucket != "" && key != "" && q.Has("uploadId") && r.Method == http.MethodPut:
		s.uploadPart(w, r, bucket, key, q.Get("uploadId"), q.Get("partNumber"))
		return
	case bucket != "" && key != "" && q.Has("uploadId") && r.Method == http.MethodPost:
		s.completeMultipartUpload(w, r, bucket, key, q.Get("uploadId"))
		return
	case bucket != "" && key != "" && q.Has("uploadId") && r.Method == http.MethodDelete:
		s.abortMultipartUpload(w, bucket, key, q.Get("uploadId"))
		return
	}

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
		s.getObject(w, r, bucket, key)
	case bucket != "" && key != "" && r.Method == http.MethodHead:
		s.headObject(w, bucket, key)
	case bucket != "" && key != "" && r.Method == http.MethodDelete:
		s.deleteObject(w, bucket, key)
	default:
		server.WriteXMLError(w, http.StatusMethodNotAllowed, "MethodNotAllowed",
			"el emulador no soporta esta combinación de método/ruta para S3")
	}
}

// Reset limpia todo el estado persistido de S3 (buckets, objetos,
// versiones, tags y uploads multipart en curso). Implementa
// server.Resettable.
func (s *Service) Reset() error {
	return s.db.Reset(
		bucketsBucket, objectsBucket, dataBucket,
		versioningBucket, versionsBucket, versionDataBucket,
		tagsBucket, uploadsBucket, partsMetaBucket, partsDataBucket,
	)
}

func isVersioningEnabled(db *storage.DB, bucket string) bool {
	var status string
	found, _ := db.Get(versioningBucket, bucket, &status)
	return found && status == "Enabled"
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
	if isVersioningEnabled(s.db, bucket) {
		meta.VersionId = randomToken(16)
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
	// Si el bucket tiene versionado habilitado, además de actualizar el
	// puntero "latest" (arriba) se guarda una copia inmutable bajo su
	// versionId, para que GET ?versionId=<id> siga sirviendo versiones
	// anteriores después de sobreescribir el objeto.
	if meta.VersionId != "" {
		vk := ok + "/" + meta.VersionId
		_ = s.db.Put(versionsBucket, vk, meta)
		_ = s.db.PutRaw(versionDataBucket, vk, body)
	}
	if meta.VersionId != "" {
		w.Header().Set("x-amz-version-id", meta.VersionId)
	}
	w.Header().Set("ETag", `"`+etag+`"`)
	server.WriteXML(w, http.StatusOK, nil)
}

func (s *Service) getObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	ok := objectKey(bucket, key)

	if versionID := r.URL.Query().Get("versionId"); versionID != "" {
		vk := ok + "/" + versionID
		var meta ObjectMeta
		found, err := s.db.Get(versionsBucket, vk, &meta)
		if err != nil || !found {
			server.WriteXMLError(w, http.StatusNotFound, "NoSuchVersion", "la versión no existe")
			return
		}
		data, found, err := s.db.GetRaw(versionDataBucket, vk)
		if err != nil || !found {
			server.WriteXMLError(w, http.StatusNotFound, "NoSuchVersion", "la versión no existe")
			return
		}
		writeObjectResponse(w, meta, data)
		return
	}

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
	writeObjectResponse(w, meta, data)
}

func writeObjectResponse(w http.ResponseWriter, meta ObjectMeta, data []byte) {
	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("ETag", `"`+meta.ETag+`"`)
	w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	if meta.VersionId != "" {
		w.Header().Set("x-amz-version-id", meta.VersionId)
	}
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

// --- versionado ---

type versioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Status  string   `xml:"Status"`
}

func (s *Service) putBucketVersioning(w http.ResponseWriter, r *http.Request, bucket string) {
	if found, _ := s.db.Get(bucketsBucket, bucket, &Bucket{}); !found {
		server.WriteXMLError(w, http.StatusNotFound, "NoSuchBucket", "el bucket no existe")
		return
	}
	var cfg versioningConfiguration
	if err := xml.NewDecoder(r.Body).Decode(&cfg); err != nil {
		server.WriteXMLError(w, http.StatusBadRequest, "InvalidRequest", "no se pudo parsear VersioningConfiguration")
		return
	}
	if err := s.db.Put(versioningBucket, bucket, cfg.Status); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, nil)
}

func (s *Service) getBucketVersioning(w http.ResponseWriter, bucket string) {
	var status string
	_, _ = s.db.Get(versioningBucket, bucket, &status)
	server.WriteXML(w, http.StatusOK, versioningConfiguration{Status: status})
}

// --- tags de objeto ---

type tagging struct {
	XMLName xml.Name `xml:"Tagging"`
	TagSet  tagSet   `xml:"TagSet"`
}
type tagSet struct {
	Tags []tagXML `xml:"Tag"`
}
type tagXML struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

func (s *Service) putObjectTagging(w http.ResponseWriter, r *http.Request, bucket, key string) {
	ok := objectKey(bucket, key)
	if found, _ := s.db.Get(objectsBucket, ok, &ObjectMeta{}); !found {
		server.WriteXMLError(w, http.StatusNotFound, "NoSuchKey", "el objeto no existe")
		return
	}
	var t tagging
	if err := xml.NewDecoder(r.Body).Decode(&t); err != nil {
		server.WriteXMLError(w, http.StatusBadRequest, "InvalidRequest", "no se pudo parsear Tagging")
		return
	}
	tags := map[string]string{}
	for _, tag := range t.TagSet.Tags {
		tags[tag.Key] = tag.Value
	}
	if err := s.db.Put(tagsBucket, ok, tags); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, nil)
}

func (s *Service) getObjectTagging(w http.ResponseWriter, bucket, key string) {
	ok := objectKey(bucket, key)
	var tags map[string]string
	_, _ = s.db.Get(tagsBucket, ok, &tags)
	var out []tagXML
	for k, v := range tags {
		out = append(out, tagXML{Key: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	server.WriteXML(w, http.StatusOK, tagging{TagSet: tagSet{Tags: out}})
}

func (s *Service) deleteObjectTagging(w http.ResponseWriter, bucket, key string) {
	ok := objectKey(bucket, key)
	_ = s.db.Delete(tagsBucket, ok)
	w.WriteHeader(http.StatusNoContent)
}

// --- multipart upload ---

type multipartUpload struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

type partMeta struct {
	ETag string `json:"etag"`
	Size int64  `json:"size"`
}

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadId string   `xml:"UploadId"`
}

func (s *Service) createMultipartUpload(w http.ResponseWriter, bucket, key string) {
	if found, _ := s.db.Get(bucketsBucket, bucket, &Bucket{}); !found {
		server.WriteXMLError(w, http.StatusNotFound, "NoSuchBucket", "el bucket no existe")
		return
	}
	uploadID := randomToken(16)
	if err := s.db.Put(uploadsBucket, uploadID, multipartUpload{Bucket: bucket, Key: key}); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, initiateMultipartUploadResult{Bucket: bucket, Key: key, UploadId: uploadID})
}

func partKey(uploadID, partNumber string) string {
	return uploadID + "/" + partNumber
}

func (s *Service) uploadPart(w http.ResponseWriter, r *http.Request, bucket, key, uploadID, partNumber string) {
	var up multipartUpload
	if found, _ := s.db.Get(uploadsBucket, uploadID, &up); !found {
		server.WriteXMLError(w, http.StatusNotFound, "NoSuchUpload", "el multipart upload no existe")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		server.WriteXMLError(w, http.StatusBadRequest, "InvalidRequest", "no se pudo leer el body")
		return
	}
	sum := md5.Sum(body)
	etag := hex.EncodeToString(sum[:])
	pk := partKey(uploadID, partNumber)
	if err := s.db.Put(partsMetaBucket, pk, partMeta{ETag: etag, Size: int64(len(body))}); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if err := s.db.PutRaw(partsDataBucket, pk, body); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.Header().Set("ETag", `"`+etag+`"`)
	w.WriteHeader(http.StatusOK)
}

type completeMultipartUploadResult struct {
	XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
	Bucket  string   `xml:"Bucket"`
	Key     string   `xml:"Key"`
	ETag    string   `xml:"ETag"`
}

// completeMultipartUpload concatena, en orden de número de parte, todos
// los chunks subidos para uploadID y los persiste como el objeto final
// — el emulador no valida tamaños mínimos de parte (5 MiB en AWS real)
// ni el orden/lista de partes enviado en el body, simplemente junta todo
// lo que se haya subido bajo ese uploadId.
func (s *Service) completeMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	var up multipartUpload
	if found, _ := s.db.Get(uploadsBucket, uploadID, &up); !found {
		server.WriteXMLError(w, http.StatusNotFound, "NoSuchUpload", "el multipart upload no existe")
		return
	}

	type partEntry struct {
		number int
		data   []byte
	}
	var parts []partEntry
	_ = s.db.List(partsDataBucket, uploadID+"/", func(k string, raw []byte) error {
		numStr := strings.TrimPrefix(k, uploadID+"/")
		num, _ := strconv.Atoi(numStr)
		parts = append(parts, partEntry{number: num, data: raw})
		return nil
	})
	sort.Slice(parts, func(i, j int) bool { return parts[i].number < parts[j].number })

	var body []byte
	for _, p := range parts {
		body = append(body, p.data...)
	}

	sum := md5.Sum(body)
	etag := hex.EncodeToString(sum[:])
	meta := ObjectMeta{
		Bucket:       bucket,
		Key:          key,
		Size:         int64(len(body)),
		ETag:         etag,
		ContentType:  "application/octet-stream",
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

	_ = s.db.Delete(uploadsBucket, uploadID)
	_ = s.db.DeletePrefix(partsMetaBucket, uploadID+"/")
	_ = s.db.DeletePrefix(partsDataBucket, uploadID+"/")

	server.WriteXML(w, http.StatusOK, completeMultipartUploadResult{Bucket: bucket, Key: key, ETag: `"` + etag + `"`})
}

func (s *Service) abortMultipartUpload(w http.ResponseWriter, bucket, key, uploadID string) {
	_ = s.db.Delete(uploadsBucket, uploadID)
	_ = s.db.DeletePrefix(partsMetaBucket, uploadID+"/")
	_ = s.db.DeletePrefix(partsDataBucket, uploadID+"/")
	w.WriteHeader(http.StatusNoContent)
}
