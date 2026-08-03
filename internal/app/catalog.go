package app

import (
	"fmt"
	"net/url"
)

const (
	defaultExecutorURL       = "https://download.xiaozhuhouses.asia/download/v1/links/4pptOkAQsdrl2CoWio-FtgZFOmhOOM9yOyB2eGV-qAk"
	defaultOpenPackageURL    = "https://download.xiaozhuhouses.asia/download/v1/links/YsxWkWgFPiZFrc8I0r2F8SpdLbhBA_O7PMnD0TDS0wM"
	defaultSponsorPackageURL = "https://download.xiaozhuhouses.asia/download/v1/links/2XehhoggMhKBjNH3jcZe4HqZ96lNgb-jFnnW95oYzsQ"
)

type Channel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	artifact    artifactSource
}

type artifactSource struct {
	URL      string
	CacheKey string
}

type ReleaseCatalog struct {
	Executor artifactSource
	channels []Channel
}

func loadReleaseCatalog() (ReleaseCatalog, error) {
	executor, err := newArtifactSource("安装脚本", envOr("QVMC_EXECUTOR_URL", defaultExecutorURL), "executor-install.sh")
	if err != nil {
		return ReleaseCatalog{}, err
	}
	openPackage, err := newArtifactSource("开源版发行包", envOr("QVMC_OPEN_PACKAGE_URL", defaultOpenPackageURL), "open-kvm-console-linux-amd64.tar.gz")
	if err != nil {
		return ReleaseCatalog{}, err
	}
	sponsorPackage, err := newArtifactSource("赞助版发行包", envOr("QVMC_SPONSOR_PACKAGE_URL", defaultSponsorPackageURL), "sponsor-kvm-console-linux-amd64.tar.gz")
	if err != nil {
		return ReleaseCatalog{}, err
	}
	return ReleaseCatalog{
		Executor: executor,
		channels: []Channel{
			{
				ID:          "open",
				Name:        "开源版",
				Description: "使用开源版发行包，适合社区用户。",
				artifact:    openPackage,
			},
			{
				ID:          "sponsor",
				Name:        "赞助版",
				Description: "使用赞助版发行包，适合已获得赞助版的用户。",
				artifact:    sponsorPackage,
			},
		},
	}, nil
}

func newArtifactSource(name, rawURL, cacheKey string) (artifactSource, error) {
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return artifactSource{}, fmt.Errorf("%s发布地址无效，必须使用不含账号信息的 HTTPS 地址", name)
	}
	return artifactSource{URL: rawURL, CacheKey: cacheKey}, nil
}

func (c ReleaseCatalog) Channels() []Channel {
	result := make([]Channel, len(c.channels))
	copy(result, c.channels)
	return result
}

func (c ReleaseCatalog) FindChannel(id string) (Channel, bool) {
	for _, channel := range c.channels {
		if channel.ID == id {
			return channel, true
		}
	}
	return Channel{}, false
}
