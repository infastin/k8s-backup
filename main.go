package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alecthomas/units"
	"github.com/caarlos0/env/v11"
	"github.com/charmbracelet/log"
	"github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/infastin/gorack/errdefer"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/applyconfigurations/autoscaling/v1"
	"k8s.io/client-go/kubernetes"
	appsv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	"k8s.io/client-go/rest"
)

type Application struct {
	clientset    *kubernetes.Clientset
	appsV1       appsv1.AppsV1Interface
	resourceType string
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

	app.appsV1 = app.clientset.AppsV1()

	resourceParts := strings.SplitN(app.config.Resource.ID, "/", 2)
	app.resourceType = resourceParts[0]
	app.resourceName = resourceParts[1]

	app.logData = new(bytes.Buffer)
	app.lg = log.New(io.MultiWriter(os.Stdout, app.logData))

	return app, nil
}

func (a *Application) Run() (err error) {
	defer func() {
		a.notify(err == nil)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	scaleUp, err := a.scaleDown(ctx)
	if err != nil {
		a.lg.Error("Failed to scale down",
			"resource", a.config.Resource.ID,
			"namespace", a.config.Resource.Namespace,
			"error", err,
		)
		return fmt.Errorf("failed to scale down: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		scaleErr := scaleUp(ctx)
		if scaleErr == nil {
			return
		}

		a.lg.Error("Failed to scale up",
			"resource", a.config.Resource.ID,
			"namespace", a.config.Resource.Namespace,
			"error", scaleErr,
		)

		scaleErr = fmt.Errorf("failed to scale up: %w", err)
		if err != nil {
			err = fmt.Errorf("%w: %w", err, scaleErr)
		} else {
			err = scaleErr
		}
	}()

	if err := a.archive(); err != nil {
		a.lg.Error("Failed to archive", "directory", a.config.Backup.Directory)
		return fmt.Errorf("failed to archive: %w", err)
	}
	defer func() {
		a.archiveFile.Close()
		if err := os.Remove(a.archiveFile.Name()); err != nil {
			a.lg.Warn("Failed to delete temporary archive file", "error", err)
		}
	}()

	if err := a.upload(context.Background()); err != nil {
		a.lg.Error("Failed to upload to S3")
		return fmt.Errorf("failed to upload to S3: %w", err)
	}

	return nil
}

func (a *Application) scaleDown(ctx context.Context) (undo func(context.Context) error, err error) {
	lg := a.lg.With(
		"resource", a.config.Resource.ID,
		"namespace", a.config.Resource.Namespace,
	)
	lg.Info("Scaling to 0")

	var scaler interface {
		GetScale(
			ctx context.Context,
			name string,
			options metav1.GetOptions,
		) (*autoscalingv1.Scale, error)

		ApplyScale(
			ctx context.Context,
			name string,
			scale *v1.ScaleApplyConfiguration,
			opts metav1.ApplyOptions,
		) (*autoscalingv1.Scale, error)
	}

	switch a.resourceType {
	case "deployment", "deployments":
		scaler = a.appsV1.Deployments(a.config.Resource.Namespace)
	case "statefulset", "statefulsets":
		scaler = a.appsV1.StatefulSets(a.config.Resource.Namespace)
	case "replicaset", "replicasets":
		scaler = a.appsV1.ReplicaSets(a.config.Resource.Namespace)
	}

	scale, err := scaler.GetScale(ctx, a.resourceName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to obtain scale information: %w", err)
	}

	if _, err := scaler.ApplyScale(ctx, a.resourceName,
		v1.Scale().WithSpec(v1.ScaleSpec().WithReplicas(0)),
		metav1.ApplyOptions{},
	); err != nil {
		return nil, fmt.Errorf("failed to scale down: %w", err)
	}

	lg.Info("Scaled to 0")

	undo = func(ctx context.Context) error {
		lg.Infof("Scaling to %d", scale.Spec.Replicas)
		if _, err := scaler.ApplyScale(ctx, a.resourceName,
			v1.Scale().WithSpec(v1.ScaleSpec().WithReplicas(scale.Spec.Replicas)),
			metav1.ApplyOptions{},
		); err != nil {
			return fmt.Errorf("failed to scale up: %w", err)
		}
		return nil
	}

	return undo, nil
}

func (a *Application) archive() (err error) {
	name := fmt.Sprintf("backup-%s.tar.gz", time.Now().Format(time.RFC3339))

	lg := a.lg.With(
		"name", name,
		"backup_directory", a.config.Backup.Directory,
	)
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

	lg.Info("Created archive", "size", units.Base2Bytes(a.archiveSize))

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
		units.Base2Bytes(p.current),
		units.Base2Bytes(p.total),
		float64(p.current)/float64(p.total)*100.0)
	return len(b), nil
}

func (a *Application) upload(ctx context.Context) (err error) {
	lg := a.lg.With(
		"endpoint", a.config.S3.Endpoint,
		"bucket", a.config.S3.Bucket,
		"name", a.archiveName,
		"file", a.archiveFile.Name(),
	)
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
				lg:      a.lg.With("name", a.archiveName),
				current: 0,
				total:   a.archiveSize,
			},
			StorageClass: a.config.S3.StorageClass,
			ContentType:  "application/tar+gzip",
			Expires:      expires,
		},
	); err != nil {
		return fmt.Errorf("failed to upload archive to S3: %w", err)
	}

	return nil
}

func (a *Application) notify(success bool) {
	if a.tgBot == nil {
		return
	}

	log.Info("Sending Telegram notification")

	var b strings.Builder
	if success {
		fmt.Fprintf(&b, "<tg-emoji>🔥</tg-emoji> Backup of %s has <b>succeeded</b>\n", a.resourceName)
	} else {
		fmt.Fprintf(&b, "<tg-emoji>🚀</tg-emoji> Backup of %s has <b>failed</b>\n", a.resourceName)
	}

	if a.archiveFile != nil {
		sz := units.Base2Bytes(a.archiveSize)
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
