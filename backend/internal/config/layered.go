package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"filetrans-backend/internal/security"
	"gopkg.in/yaml.v3"
)

var ErrLegacyConfig = errors.New("旧版配置仅支持读取，请先停止服务并运行 -migrate-config -config 配置路径")

type configSource struct {
	legacy       bool
	settingsPath string
	document     *yaml.Node
	baseline     *Config
}

func (c *Config) Legacy() bool { return c.source != nil && c.source.legacy }

func (c *Config) ResolvePath(path string) string {
	if path != "" && !filepath.IsAbs(path) && c.source != nil && !c.source.legacy {
		return filepath.Clean(filepath.Join(filepath.Dir(c.source.settingsPath), path))
	}
	return path
}

func readYAML(path string) (*yaml.Node, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取配置 %s：%w", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("配置格式错误 %s：%w", path, err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("配置 %s 顶层必须为映射", path)
	}
	return &doc, nil
}

func nodeField(n *yaml.Node, key string) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.DocumentNode {
		n = n.Content[0]
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func removeNodeField(n *yaml.Node, key string) {
	if n.Kind == yaml.DocumentNode {
		n = n.Content[0]
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			n.Content = append(n.Content[:i], n.Content[i+2:]...)
			return
		}
	}
}

func loadLayered(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	doc, err := readYAML(abs)
	if err != nil {
		return nil, err
	}
	c := Default()
	src := &configSource{legacy: nodeField(doc, "admin") == nil, document: doc}
	if src.legacy {
		if err := doc.Decode(c); err != nil {
			return nil, fmt.Errorf("旧版配置格式错误：%w", err)
		}
	} else {
		var bootstrap struct {
			Admin AdminConfig `yaml:"admin"`
		}
		if err := doc.Decode(&bootstrap); err != nil {
			return nil, err
		}
		if bootstrap.Admin.PasswordSHA256 != "" || security.Validate(bootstrap.Admin.PasswordHash) != nil {
			return nil, fmt.Errorf("bootstrap admin.password_hash 必须为有效 Argon2id 哈希，请使用密码工具 --format bootstrap")
		}
		src.settingsPath = filepath.Join(filepath.Dir(abs), "settings.yaml")
		settings, err := readYAML(src.settingsPath)
		if err != nil {
			return nil, fmt.Errorf("需要 bootstrap 同目录的 settings.yaml：%w", err)
		}
		if nodeField(settings, "admin") != nil || nodeField(nodeField(settings, "auth"), "admin") != nil {
			return nil, fmt.Errorf("settings.yaml 不得包含管理员凭据，请仅在 bootstrap 的顶层 admin 中配置")
		}
		if err := settings.Decode(c); err != nil {
			return nil, fmt.Errorf("settings.yaml 格式错误：%w", err)
		}
		c.Auth.Admin = bootstrap.Admin
		src.document = settings
	}
	c.normalize()
	if !src.legacy {
		resolveConfigPaths(c, filepath.Dir(abs))
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("配置内容校验失败（%s）：%w", abs, err)
	}
	src.baseline = c.Clone()
	c.source = src
	return c, nil
}

func resolveConfigPaths(c *Config, base string) {
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Clean(filepath.Join(base, p))
	}
	c.Database.Path = resolve(c.Database.Path)
	c.Web.StaticDir = resolve(c.Web.StaticDir)
	for i := range c.Storage.Dirs {
		c.Storage.Dirs[i].Path = resolve(c.Storage.Dirs[i].Path)
	}
	for i := range c.Storage.Shares {
		c.Storage.Shares[i].Path = resolve(c.Storage.Shares[i].Path)
	}
	for i := range c.FilePicker.Roots {
		c.FilePicker.Roots[i].Path = resolve(c.FilePicker.Roots[i].Path)
	}
}

func configNode(c *Config) (*yaml.Node, error) {
	var n yaml.Node
	if err := n.Encode(c); err != nil {
		return nil, err
	}
	if auth := nodeField(&n, "auth"); auth != nil {
		removeNodeField(auth, "admin")
	}
	return &n, nil
}

func cloneNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	out := *n
	out.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		out.Content[i] = cloneNode(child)
	}
	return &out
}

func equalNode(a, b *yaml.Node) bool {
	if a == nil || b == nil {
		return a == b
	}
	var av, bv any
	if a.Decode(&av) != nil || b.Decode(&bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// Only changed known nodes are written. Missing defaults, explicit zero/false,
// comments and extension keys remain untouched; resource list items retain extras by id.
func mergeConfigNode(original, before, after *yaml.Node) *yaml.Node {
	if equalNode(before, after) {
		return cloneNode(original)
	}
	if original == nil {
		return cloneNode(after)
	}
	if after.Kind == yaml.MappingNode && original.Kind == yaml.MappingNode {
		out := cloneNode(original)
		for i := 0; i+1 < len(after.Content); i += 2 {
			key, value := after.Content[i], after.Content[i+1]
			old := nodeField(before, key.Value)
			if equalNode(old, value) {
				continue
			}
			merged := mergeConfigNode(nodeField(original, key.Value), old, value)
			found := false
			for j := 0; j+1 < len(out.Content); j += 2 {
				if out.Content[j].Value == key.Value {
					out.Content[j+1] = merged
					found = true
					break
				}
			}
			if !found {
				out.Content = append(out.Content, cloneNode(key), merged)
			}
		}
		return out
	}
	if after.Kind == yaml.SequenceNode && original.Kind == yaml.SequenceNode {
		out := cloneNode(after)
		for i, item := range after.Content {
			id := nodeField(item, "id")
			if id == nil {
				continue
			}
			var old, orig *yaml.Node
			if before != nil {
				for _, x := range before.Content {
					if xid := nodeField(x, "id"); xid != nil && xid.Value == id.Value {
						old = x
						break
					}
				}
			}
			for _, x := range original.Content {
				if xid := nodeField(x, "id"); xid != nil && xid.Value == id.Value {
					orig = x
					break
				}
			}
			out.Content[i] = mergeConfigNode(orig, old, item)
		}
		return out
	}
	out := cloneNode(after)
	out.HeadComment = original.HeadComment
	out.LineComment = original.LineComment
	out.FootComment = original.FootComment
	return out
}

func settingsForSave(c *Config) ([]byte, error) {
	if c.source == nil {
		return yaml.Marshal(c)
	} // In-memory/embedding callers keep the legacy serializer.
	after, err := configNode(c)
	if err != nil {
		return nil, err
	}
	before, err := configNode(c.source.baseline)
	if err != nil {
		return nil, err
	}
	doc := cloneNode(c.source.document)
	// Resource edits can move an item between dirs and shares. Carry its original
	// extension nodes across that move rather than treating it as a brand-new item.
	originalStorage := nodeField(doc, "storage")
	if originalStorage != nil {
		byID := map[string]*yaml.Node{}
		for _, key := range []string{"dirs", "shares"} {
			if seq := nodeField(originalStorage, key); seq != nil {
				for _, item := range seq.Content {
					if id := nodeField(item, "id"); id != nil {
						byID[id.Value] = cloneNode(item)
					}
				}
			}
		}
		for _, key := range []string{"dirs", "shares"} {
			seq := nodeField(nodeField(after, "storage"), key)
			if seq == nil {
				continue
			}
			for _, item := range seq.Content {
				id := nodeField(item, "id")
				if id == nil || byID[id.Value] == nil {
					continue
				}
				originalSeq := nodeField(originalStorage, key)
				found := false
				if originalSeq != nil {
					for _, old := range originalSeq.Content {
						if oid := nodeField(old, "id"); oid != nil && oid.Value == id.Value {
							found = true
							break
						}
					}
				}
				if found {
					continue
				}
				if originalSeq == nil {
					originalSeq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
					originalStorage.Content = append(originalStorage.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, originalSeq)
				}
				originalSeq.Kind = yaml.SequenceNode
				originalSeq.Tag = "!!seq"
				originalSeq.Value = ""
				originalSeq.Content = append(originalSeq.Content, cloneNode(byID[id.Value]))
			}
		}
	}
	doc.Content[0] = mergeConfigNode(doc.Content[0], before, after)
	b, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	baseline := c.Clone()
	baseline.source = nil
	c.source = &configSource{settingsPath: c.source.settingsPath, document: doc, baseline: baseline}
	return b, nil
}

// MigrateConfig is offline only. It never opens the database. The original file
// is backed up once, settings are validated and durably published first, then
// bootstrap is switched last. Identical settings permit recovery after interruption.
func MigrateConfig(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	c, err := Load(abs)
	if err != nil {
		return err
	}
	if !c.Legacy() {
		return nil
	}
	if err := security.Validate(c.Auth.Admin.PasswordHash); err != nil {
		return fmt.Errorf("旧配置只有 SHA256 或无有效 Argon2id；请先用密码工具生成有效 password_hash，不能从 SHA256 转换")
	}
	original, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	// Reading legacy configurations stays permissive. Migration cannot infer the
	// ownership or relative-path semantics of unknown fields, so reject them before
	// creating a backup or publishing either new file. KnownFields reports field
	// names, containing types and source lines, not their potentially secret values.
	decoder := yaml.NewDecoder(bytes.NewReader(original))
	decoder.KnownFields(true)
	var strictConfig Config
	if err := decoder.Decode(&strictConfig); err != nil {
		return fmt.Errorf("旧版配置包含无法自动归属的字段，迁移已停止（配置文件：%s）；请按字段及行号人工处理：%w", abs, err)
	}
	backup := abs + ".pre-migration.bak"
	if b, err := os.ReadFile(backup); err == nil {
		if !bytes.Equal(b, original) {
			return fmt.Errorf("迁移备份冲突：%s，请人工确认", backup)
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if err := writeFileAtomic(backup, original, 0600); err != nil {
		return err
	}
	base, err := os.Getwd()
	if err != nil {
		return err
	}
	resolveConfigPaths(c, base) // Preserve legacy cwd-relative runtime destinations.
	before, err := configNode(c.source.baseline)
	if err != nil {
		return err
	}
	after, err := configNode(c)
	if err != nil {
		return err
	}
	settings := cloneNode(c.source.document)
	if auth := nodeField(settings, "auth"); auth != nil {
		removeNodeField(auth, "admin")
	}
	settings.Content[0] = mergeConfigNode(settings.Content[0], before, after)
	settingsBytes, err := yaml.Marshal(settings)
	if err != nil {
		return err
	}
	check := Default()
	if err := settings.Decode(check); err != nil {
		return err
	}
	check.Auth.Admin = c.Auth.Admin
	check.normalize()
	if err := check.validate(); err != nil {
		return fmt.Errorf("迁移后 settings 校验失败：%w", err)
	}
	settingsPath := filepath.Join(filepath.Dir(abs), "settings.yaml")
	if old, err := os.ReadFile(settingsPath); err == nil {
		if !bytes.Equal(old, settingsBytes) {
			return fmt.Errorf("settings.yaml 已存在且内容冲突，未覆盖：%s", settingsPath)
		}
		f, err := os.OpenFile(settingsPath, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(abs)); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if err := writeFileAtomic(settingsPath, settingsBytes, 0600); err != nil {
		return err
	}
	bootstrap := cloneNode(c.source.document)
	// Strictly checked service settings live only in settings.
	known, err := configNode(c)
	if err != nil {
		return err
	}
	for i := 0; i+1 < len(known.Content); i += 2 {
		removeNodeField(bootstrap, known.Content[i].Value)
	}
	var admin yaml.Node
	if err := admin.Encode(c.Auth.Admin); err != nil {
		return err
	}
	if originalAdmin := nodeField(nodeField(c.source.document, "auth"), "admin"); originalAdmin != nil {
		admin = *mergeConfigNode(originalAdmin, nil, &admin)
	}
	removeNodeField(&admin, "password_sha256")
	bootstrap.Content[0].Content = append(bootstrap.Content[0].Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "admin"}, &admin)
	b, err := yaml.Marshal(bootstrap)
	if err != nil {
		return err
	}
	return writeFileAtomic(abs, b, 0600)
}
