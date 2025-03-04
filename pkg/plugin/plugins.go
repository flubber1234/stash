// Package plugin implements functions and types for maintaining and running
// stash plugins.
//
// Stash plugins are configured using yml files in the configured plugins
// directory. These yml files must follow the Config structure format.
//
// The main entry into the plugin sub-system is via the Cache type.
package plugin

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/stashapp/stash/pkg/fsutil"
	"github.com/stashapp/stash/pkg/logger"
	"github.com/stashapp/stash/pkg/models"
	"github.com/stashapp/stash/pkg/plugin/common"
	"github.com/stashapp/stash/pkg/session"
	"github.com/stashapp/stash/pkg/sliceutil"
	"github.com/stashapp/stash/pkg/txn"
	"github.com/stashapp/stash/pkg/utils"
)

type Plugin struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description *string         `json:"description"`
	URL         *string         `json:"url"`
	Version     *string         `json:"version"`
	Tasks       []*PluginTask   `json:"tasks"`
	Hooks       []*PluginHook   `json:"hooks"`
	UI          PluginUI        `json:"ui"`
	Settings    []PluginSetting `json:"settings"`

	Enabled bool `json:"enabled"`

	// ConfigPath is the path to the plugin's configuration file.
	ConfigPath string `json:"-"`
}

type PluginUI struct {
	// Requires is a list of plugin IDs that this plugin depends on.
	// These plugins will be loaded before this plugin.
	Requires []string `json:"requires"`

	// Content Security Policy configuration for the plugin.
	CSP PluginCSP `json:"csp"`

	// External Javascript files that will be injected into the stash UI.
	ExternalScript []string `json:"external_script"`

	// External CSS files that will be injected into the stash UI.
	ExternalCSS []string `json:"external_css"`

	// Javascript files that will be injected into the stash UI.
	Javascript []string `json:"javascript"`

	// CSS files that will be injected into the stash UI.
	CSS []string `json:"css"`

	// Assets is a map of URL prefixes to hosted directories.
	// This allows plugins to serve static assets from a URL path.
	// Plugin assets are exposed via the /plugin/{pluginId}/assets path.
	// For example, if the plugin configuration file contains:
	// /foo: bar
	// /bar: baz
	// /: root
	// Then the following requests will be mapped to the following files:
	// /plugin/{pluginId}/assets/foo/file.txt -> {pluginDir}/foo/file.txt
	// /plugin/{pluginId}/assets/bar/file.txt -> {pluginDir}/baz/file.txt
	// /plugin/{pluginId}/assets/file.txt -> {pluginDir}/root/file.txt
	Assets utils.URLMap `json:"assets"`
}

type PluginSetting struct {
	Name string `json:"name"`
	// defaults to string
	Type PluginSettingTypeEnum `json:"type"`
	// defaults to key name
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
}

type ServerConfig interface {
	GetHost() string
	GetPort() int
	GetConfigPath() string
	HasTLSConfig() bool
	GetPluginsPath() string
	GetDisabledPlugins() []string
	GetPythonPath() string
}

// Cache stores plugin details.
type Cache struct {
	config       ServerConfig
	plugins      []Config
	sessionStore *session.Store
	gqlHandler   http.Handler
}

// NewCache returns a new Cache.
//
// Plugins configurations are loaded from yml files in the plugin
// directory in the config and any subdirectories.
//
// Does not load plugins. Plugins will need to be
// loaded explicitly using ReloadPlugins.
func NewCache(config ServerConfig) *Cache {
	return &Cache{
		config: config,
	}
}

func (c *Cache) RegisterGQLHandler(handler http.Handler) {
	c.gqlHandler = handler
}

func (c *Cache) RegisterSessionStore(sessionStore *session.Store) {
	c.sessionStore = sessionStore
}

// ReloadPlugins clears the plugin cache and loads from the plugin path.
// If a plugin cannot be loaded, an error is logged and the plugin is skipped.
func (c *Cache) ReloadPlugins() {
	path := c.config.GetPluginsPath()
	plugins := make([]Config, 0)

	logger.Debugf("Reading plugin configs from %s", path)

	err := fsutil.SymWalk(path, func(fp string, f os.FileInfo, err error) error {
		if filepath.Ext(fp) == ".yml" {
			plugin, err := loadPluginFromYAMLFile(fp)
			if err != nil {
				logger.Errorf("Error loading plugin %s: %v", fp, err)
			} else {
				plugins = append(plugins, *plugin)
			}
		}
		return nil
	})

	if err != nil {
		logger.Errorf("Error reading plugin configs: %v", err)
	}

	c.plugins = plugins
}

func (c Cache) enabledPlugins() []Config {
	disabledPlugins := c.config.GetDisabledPlugins()

	var ret []Config
	for _, p := range c.plugins {
		disabled := sliceutil.Contains(disabledPlugins, p.id)

		if !disabled {
			ret = append(ret, p)
		}
	}

	return ret
}

func (c Cache) pluginDisabled(id string) bool {
	disabledPlugins := c.config.GetDisabledPlugins()

	return sliceutil.Contains(disabledPlugins, id)
}

// ListPlugins returns plugin details for all of the loaded plugins.
func (c Cache) ListPlugins() []*Plugin {
	disabledPlugins := c.config.GetDisabledPlugins()

	var ret []*Plugin
	for _, s := range c.plugins {
		p := s.toPlugin()

		disabled := sliceutil.Contains(disabledPlugins, p.ID)
		p.Enabled = !disabled

		ret = append(ret, p)
	}

	return ret
}

// GetPlugin returns the plugin with the given ID.
// Returns nil if the plugin is not found.
func (c Cache) GetPlugin(id string) *Plugin {
	disabledPlugins := c.config.GetDisabledPlugins()
	plugin := c.getPlugin(id)
	if plugin != nil {
		p := plugin.toPlugin()

		disabled := sliceutil.Contains(disabledPlugins, p.ID)
		p.Enabled = !disabled
		return p
	}

	return nil
}

// ListPluginTasks returns all runnable plugin tasks in all loaded plugins.
func (c Cache) ListPluginTasks() []*PluginTask {
	var ret []*PluginTask
	for _, s := range c.enabledPlugins() {
		ret = append(ret, s.getPluginTasks(true)...)
	}

	return ret
}

func buildPluginInput(plugin *Config, operation *OperationConfig, serverConnection common.StashServerConnection, args []*PluginArgInput) common.PluginInput {
	args = applyDefaultArgs(args, operation.DefaultArgs)
	serverConnection.PluginDir = plugin.getConfigPath()
	return common.PluginInput{
		ServerConnection: serverConnection,
		Args:             toPluginArgs(args),
	}
}

func (c Cache) makeServerConnection(ctx context.Context) common.StashServerConnection {
	cookie := c.sessionStore.MakePluginCookie(ctx)

	serverConnection := common.StashServerConnection{
		Scheme:        "http",
		Host:          c.config.GetHost(),
		Port:          c.config.GetPort(),
		SessionCookie: cookie,
		Dir:           c.config.GetConfigPath(),
	}

	if c.config.HasTLSConfig() {
		serverConnection.Scheme = "https"
	}

	return serverConnection
}

// CreateTask runs the plugin operation for the pluginID and operation
// name provided. Returns an error if the plugin or the operation could not be
// resolved.
func (c Cache) CreateTask(ctx context.Context, pluginID string, operationName string, args []*PluginArgInput, progress chan float64) (Task, error) {
	serverConnection := c.makeServerConnection(ctx)

	if c.pluginDisabled(pluginID) {
		return nil, fmt.Errorf("plugin %s is disabled", pluginID)
	}

	// find the plugin and operation
	plugin := c.getPlugin(pluginID)

	if plugin == nil {
		return nil, fmt.Errorf("no plugin with ID %s", pluginID)
	}

	operation := plugin.getTask(operationName)
	if operation == nil {
		return nil, fmt.Errorf("no task with name %s in plugin %s", operationName, plugin.getName())
	}

	task := pluginTask{
		plugin:       plugin,
		operation:    operation,
		input:        buildPluginInput(plugin, operation, serverConnection, args),
		progress:     progress,
		gqlHandler:   c.gqlHandler,
		serverConfig: c.config,
	}
	return task.createTask(), nil
}

func (c Cache) ExecutePostHooks(ctx context.Context, id int, hookType HookTriggerEnum, input interface{}, inputFields []string) {
	if err := c.executePostHooks(ctx, hookType, common.HookContext{
		ID:          id,
		Type:        hookType.String(),
		Input:       input,
		InputFields: inputFields,
	}); err != nil {
		logger.Errorf("error executing post hooks: %s", err.Error())
	}
}

func (c Cache) RegisterPostHooks(ctx context.Context, id int, hookType HookTriggerEnum, input interface{}, inputFields []string) {
	txn.AddPostCommitHook(ctx, func(ctx context.Context) {
		c.ExecutePostHooks(ctx, id, hookType, input, inputFields)
	})
}

func (c Cache) ExecuteSceneUpdatePostHooks(ctx context.Context, input models.SceneUpdateInput, inputFields []string) {
	id, err := strconv.Atoi(input.ID)
	if err != nil {
		logger.Errorf("error converting id in SceneUpdatePostHooks: %v", err)
		return
	}
	c.ExecutePostHooks(ctx, id, SceneUpdatePost, input, inputFields)
}

func (c Cache) executePostHooks(ctx context.Context, hookType HookTriggerEnum, hookContext common.HookContext) error {
	visitedPlugins := session.GetVisitedPlugins(ctx)

	for _, p := range c.enabledPlugins() {
		hooks := p.getHooks(hookType)
		// don't revisit a plugin we've already visited
		// only log if there's hooks that we're skipping
		if len(hooks) > 0 && sliceutil.Contains(visitedPlugins, p.id) {
			logger.Debugf("plugin ID '%s' already triggered, not re-triggering", p.id)
			continue
		}

		for _, h := range hooks {
			newCtx := session.AddVisitedPlugin(ctx, p.id)
			serverConnection := c.makeServerConnection(newCtx)

			pluginInput := buildPluginInput(&p, &h.OperationConfig, serverConnection, nil)
			addHookContext(pluginInput.Args, hookContext)

			pt := pluginTask{
				plugin:       &p,
				operation:    &h.OperationConfig,
				input:        pluginInput,
				gqlHandler:   c.gqlHandler,
				serverConfig: c.config,
			}

			task := pt.createTask()
			if err := task.Start(); err != nil {
				return err
			}

			// handle cancel from context
			c := make(chan struct{})
			go func() {
				task.Wait()
				close(c)
			}()

			select {
			case <-ctx.Done():
				if err := task.Stop(); err != nil {
					logger.Warnf("could not stop task: %v", err)
				}
				return fmt.Errorf("operation cancelled")
			case <-c:
				// task finished normally
			}

			output := task.GetResult()
			if output == nil {
				logger.Debugf("%s [%s]: returned no result", hookType.String(), p.Name)
			} else {
				if output.Error != nil {
					logger.Errorf("%s [%s]: returned error: %s", hookType.String(), p.Name, *output.Error)
				} else if output.Output != nil {
					logger.Debugf("%s [%s]: returned: %v", hookType.String(), p.Name, output.Output)
				}
			}
		}
	}

	return nil
}

func (c Cache) getPlugin(pluginID string) *Config {
	for _, s := range c.plugins {
		if s.id == pluginID {
			return &s
		}
	}

	return nil
}
