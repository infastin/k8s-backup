package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/charmbracelet/log"
	"github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/infastin/gorack/errdefer"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type Application struct {
	clientset    *kubernetes.Clientset
	resourceType string
	resourceKind string
	resourceName string
	config       Config
	tgBot        *tgbotapi.BotAPI
	s3Client     *minio.Client
	lg           *log.Logger
	logData      *bytes.Buffer
	archiveName  string
	archiveFile  *os.File
	archiveSize  int64
}

func NewApplication() (app *Application, err error) {
	app = new(Application)

	if err := env.Parse(&app.config); err != nil {
		return nil, fmt.Errorf("failed to parse environment variables: %w", err)
	}

	if err := app.config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to obtain k8s config: %w", err)
	}

	app.clientset, err = kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}

	if app.config.Telegram.BotToken != "" {
		app.tgBot, err = tgbotapi.NewBotAPI(app.config.Telegram.BotToken)
		if err != nil {
			return nil, fmt.Errorf("failed to create Telegram Bot API: %w", err)
		}
	}

	app.s3Client, err = minio.New(app.config.S3.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(app.config.S3.AccessKeyID, app.config.S3.SecretAccessKey, ""),
		Secure: !app.config.S3.Unsecure,
		Region: app.config.S3.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create S3 client: %w", err)
	}

	resourceParts := strings.SplitN(app.config.Resource.ID, "/", 2)
	app.resourceName = resourceParts[1]
	switch resourceParts[0] {
	case "deployment", "deployments":
		app.resourceType = "deployments"
		app.resourceKind = "Deployment"
	case "statefulset", "statefulsets":
		app.resourceType = "statefulsets"
		app.resourceKind = "StatefulSet"
	case "replicaset", "replicasets":
		app.resourceType = "replicasets"
		app.resourceKind = "ReplicaSet"
	}

	app.logData = new(bytes.Buffer)
	app.lg = log.NewWithOptions(io.MultiWriter(os.Stdout, app.logData), log.Options{
		ReportTimestamp: true,
		Formatter:       log.TextFormatter,
	})

	return app, nil
}

func (a *Application) Run() (err error) {
	defer func() {
		a.notify(err == nil)
	}()

	lg := a.lg.With(
		"resource", a.config.Resource.ID,
		"namespace", a.config.Resource.Namespace,
	)

	ctx := log.WithContext(context.Background(), lg)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	scaleUp, err := a.scaleDown(ctx)
	if err != nil {
		lg.Error("Failed to scale down", "error", err)
		return fmt.Errorf("failed to scale down: %w", err)
	}
	defer func() {
		lg := a.lg.With(
			"resource", a.config.Resource.ID,
			"namespace", a.config.Resource.Namespace,
		)

		ctx := log.WithContext(context.Background(), lg)
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()

		scaleErr := scaleUp(ctx)
		if scaleErr == nil {
			return
		}

		lg.Error("Failed to scale up", "error", scaleErr)

		scaleErr = fmt.Errorf("failed to scale up: %w", err)
		if err != nil {
			err = fmt.Errorf("%w: %w", err, scaleErr)
		} else {
			err = scaleErr
		}
	}()

	lg = a.lg.With("directory", a.config.Backup.Directory)
	ctx = log.WithContext(context.Background(), lg)

	if err := a.archive(ctx); err != nil {
		lg.Error("Failed to archive", "error", err)
		return fmt.Errorf("failed to archive: %w", err)
	}
	defer func() {
		a.archiveFile.Close()
		if err := os.Remove(a.archiveFile.Name()); err != nil {
			a.lg.Warn("Failed to delete temporary archive file", "error", err)
		}
	}()

	lg = a.lg.With(
		"endpoint", a.config.S3.Endpoint,
		"bucket", a.config.S3.Bucket,
		"name", a.archiveName,
		"file", a.archiveFile.Name(),
	)
	ctx = log.WithContext(context.Background(), lg)

	if err := a.upload(ctx); err != nil {
		a.lg.Error("Failed to upload to S3", "error", err)
		return fmt.Errorf("failed to upload to S3: %w", err)
	}

	return nil
}

func (a *Application) getPodTemplateHash(ctx context.Context) (hash string, err error) {
	lg := log.FromContext(ctx)
	lg.Info("Trying to get pod template hash")

	appsV1 := a.clientset.AppsV1()
	replicasets := appsV1.ReplicaSets(a.config.Resource.Namespace)

	var replicaset *appsv1.ReplicaSet
	if a.resourceName != "replicasets" {
		list, err := replicasets.List(ctx, metav1.ListOptions{})
		if err != nil {
			return "", fmt.Errorf("failed to list replicasets: %w", err)
		}
		for i := range list.Items {
			item := &list.Items[i]
			for _, ref := range item.OwnerReferences {
				if ref.Kind == a.resourceKind && ref.Name == a.resourceName {
					replicaset = item
					break
				}
			}
		}
	} else {
		replicaset, err = replicasets.Get(ctx, a.resourceName, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("failed to get replicaset: %w", err)
		}
	}

	hash = replicaset.Labels["pod-template-hash"]
	lg.Info("Got pod template hash", "hash", hash)

	return hash, nil
}

type (
	objectForReplicas struct {
		Replicas int `json:"replicas"`
	}

	objectForSpec struct {
		Spec objectForReplicas `json:"spec"`
	}
)

func (a *Application) getReplicas(ctx context.Context) (replicas int, err error) {
	lg := log.FromContext(ctx)
	lg.Infof("Trying to get current number of replicas")

	data, err := a.clientset.AppsV1().RESTClient().
		Get().
		Namespace(a.config.Resource.Namespace).
		Resource(a.resourceType).
		Name(a.resourceName).
		SubResource("scale").
		DoRaw(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get resource: %w", err)
	}

	var obj objectForSpec
	if err := json.Unmarshal(data, &obj); err != nil {
		return 0, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	replicas = obj.Spec.Replicas
	lg.Info("Got number of replicas", "count", replicas)

	return replicas, nil
}

func (a *Application) scale(ctx context.Context, replicas int) (err error) {
	lg := log.FromContext(ctx)
	lg.Infof("Trying to scale to %d", replicas)

	spec := objectForSpec{
		Spec: objectForReplicas{Replicas: replicas},
	}

	patch, err := json.Marshal(&spec)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	_, err = a.clientset.AppsV1().RESTClient().
		Patch(types.MergePatchType).
		Namespace(a.config.Resource.Namespace).
		Resource(a.resourceType).
		Name(a.resourceName).
		SubResource("scale").
		Body(patch).
		DoRaw(ctx)
	if err != nil {
		return fmt.Errorf("failed to scale to %d: %w", replicas, err)
	}

	lg.Infof("Successfuly scaled to %d", replicas)

	return nil
}

func (a *Application) wait(ctx context.Context) (err error) {
	lg := log.FromContext(ctx)
	lg.Info("Waiting for pods to terminate")

	hash, err := a.getPodTemplateHash(ctx)
	if err != nil {
		return fmt.Errorf("failed to get pod template hash: %w", err)
	}
	selector := fmt.Sprintf("pod-template-hash=%s", hash)

	for {
		list, err := a.clientset.CoreV1().
			Pods(a.config.Resource.Namespace).
			List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return fmt.Errorf("failed to list pods: %w", err)
		}

		if len(list.Items) == 0 {
			break
		}
		time.Sleep(5 * time.Second)
	}

	lg.Info("Pods have terminated")

	return nil
}

func (a *Application) scaleDown(ctx context.Context) (undo func(context.Context) error, err error) {
	replicas, err := a.getReplicas(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get current number of replicas: %w", err)
	}

	if err := a.scale(ctx, 0); err != nil {
		return nil, fmt.Errorf("failed to scale down: %w", err)
	}

	if a.config.Resource.Wait {
		if err := a.wait(ctx); err != nil {
			a.lg.Warn("Failed to wait for pods to terminate", "error", err)
		}
	}

	undo = func(ctx context.Context) error {
		if err := a.scale(ctx, replicas); err != nil {
			return fmt.Errorf("failed to scale up: %w", err)
		}
		return nil
	}

	return undo, nil
}

func (a *Application) archive(ctx context.Context) (err error) {
	name := fmt.Sprintf("backup-%s.tar.gz", time.Now().Format(time.RFC3339))

	lg := log.FromContext(ctx).With("name", name)
	lg.Info("Creating archive")

	file, err := os.Create(filepath.Join(os.TempDir(), name))
	if err != nil {
		return fmt.Errorf("failed to create archive file: %w", err)
	}
	defer errdefer.Close(&err, file.Close)

	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)

	if err := tarWriter.AddFS(os.DirFS(a.config.Backup.Directory)); err != nil {
		return fmt.Errorf("failed to archive directory: %w", err)
	}

	if err := tarWriter.Close(); err != nil {
		return fmt.Errorf("failed to close tar writer: %w", err)
	}

	if err := gzipWriter.Close(); err != nil {
		return fmt.Errorf("failed to close gzip writer: %w", err)
	}

	fileInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to get archive info: %w", err)
	}

	a.archiveName = name
	a.archiveFile = file
	a.archiveSize = fileInfo.Size()

	lg.Info("Created archive", "size", byteCountIEC(a.archiveSize))

	return nil
}

type uploadProgress struct {
	lg      *log.Logger
	current int64
	total   int64
}

func (p *uploadProgress) Read(b []byte) (n int, err error) {
	p.current += int64(len(b))
	p.lg.Infof("Uploaded %s / %s (%.2f)",
		byteCountIEC(p.current),
		byteCountIEC(p.total),
		float64(p.current)/float64(p.total)*100.0)
	return len(b), nil
}

func (a *Application) upload(ctx context.Context) (err error) {
	lg := log.FromContext(ctx)
	lg.Info("Uploading archive to S3")

	var expires time.Time
	if a.config.S3.ArchiveLifetime != 0 {
		expires = time.Now().Add(time.Duration(a.config.S3.ArchiveLifetime))
	}

	if _, err := a.s3Client.PutObject(ctx,
		a.config.S3.Bucket,
		a.archiveName,
		a.archiveFile,
		a.archiveSize,
		minio.PutObjectOptions{
			Progress: &uploadProgress{
				lg:      log.With("name", a.archiveName),
				current: 0,
				total:   a.archiveSize,
			},
			StorageClass: a.config.S3.StorageClass,
			ContentType:  "application/gzip",
			Expires:      expires,
		},
	); err != nil {
		return fmt.Errorf("failed to upload archive to S3: %w", err)
	}

	lg.Info("Uploaded archive to S3")

	return nil
}

func (a *Application) notify(success bool) {
	if a.tgBot == nil {
		return
	}

	log.Info("Sending Telegram notification")

	var b strings.Builder
	if success {
		fmt.Fprintf(&b, "<tg-emoji emoji-id=\"5431815452437257407\">🐳</tg-emoji> Backup of %s has <b>succeeded</b>\n", a.resourceName)
	} else {
		fmt.Fprintf(&b, "<tg-emoji emoji-id=\"5370869711888194012\">👾</tg-emoji> Backup of %s has <b>failed</b>\n", a.resourceName)
	}

	if a.archiveFile != nil {
		sz := byteCountIEC(a.archiveSize)
		fmt.Fprintf(&b, "Tarball size: %s\n", sz)
	}

	b.WriteString("\nLog output was:\n<pre>")
	io.Copy(&b, a.logData)
	b.WriteString("</pre>")

	if _, err := a.tgBot.Send(tgbotapi.MessageConfig{
		BaseChat: tgbotapi.BaseChat{
			ChatID:           a.config.Telegram.ChatID,
			ReplyToMessageID: 0,
		},
		Text:      b.String(),
		ParseMode: "HTML",
	}); err != nil {
		log.Error("Failed to send Telegram notification", "error", err)
	}
}

func byteCountIEC(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB",
		float64(b)/float64(div), "KMGTPE"[exp])
}

func main() {
	app, err := NewApplication()
	if err != nil {
		log.Error("Failed to setup application", "error", err)
		os.Exit(1)
	}

	if err := app.Run(); err != nil {
		log.Error("Failed to run application", "error", err)
		os.Exit(1)
	}
}
