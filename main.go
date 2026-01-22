package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/k3a/html2text"
	"github.com/pkg/browser"
	"github.com/zalando/go-keyring"
	miniflux "miniflux.app/v2/client"
)

// App States
const (
	StateReading = iota
	StateBrowsing
	StateSearching
	StateYouTubeLink
	StateLogin
	StateHelp
)

// Search Modes
const (
	SearchGeneral = iota
	SearchFeed
	SearchAuthor
	SearchCategory
	SearchTags
)

var searchModes = []string{"General", "Blog Title", "Author", "Category", "Tags"}

// Styles
var (
	bgColor = lipgloss.Color("") // Initial background is terminal default

	focusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")). // Red
			Background(bgColor).
			Bold(true)

	normalStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("255")). // White
			Background(bgColor)

	hudStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")). // Grey
			Background(bgColor).
			Align(lipgloss.Center)

	lineStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("238")). // Dark Grey
			Background(bgColor)

	appStyle = lipgloss.NewStyle().
			Background(bgColor).
			Foreground(lipgloss.Color("#FFFFFF"))

	listSelectedStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("196")).
				Bold(true)

	// Theme Management
	currentTheme = 0
	themes       = []lipgloss.Color{
		lipgloss.Color(""),        // Default terminal background
		lipgloss.Color("#000000"), // Black
		lipgloss.Color("#1e1e2e"), // Catppuccin Mocha
		lipgloss.Color("#282c34"), // One Dark
		lipgloss.Color("#fbf1c7"), // Gruvbox Light
		lipgloss.Color("#ffffff"), // White
	}
)

type model struct {
	state         int
	content       []string
	index         int
	wpm           int
	paused        bool
	largeText     bool
	rampSpeed     bool
	zenMode       bool
	width         int
	height        int
	previousState int
	err           error

	// Miniflux
	minifluxClient *miniflux.Client
	entries        []*miniflux.Entry
	totalEntries   int
	fetchingMore   bool
	cursor         int
	loading        bool
	currentEntry   *miniflux.Entry
	listOffset     int // For scrolling in browsing mode
	searchInput    textinput.Model
	urlInput       textinput.Model // For Miniflux URL input

	// Search
	searchMode   int
	categories   miniflux.Categories
	feeds        miniflux.Feeds
	filteredList []string
	filteredIDs  []int64
	searchCursor int

	// Statistics
	// Statistics
	sessionArticles int
	sessionWords    int

	// Filters
	filterYouTube     bool
	currentCategoryID int64
	currentFeedID     int64

	// Configuration
	cfg Config
}

type tickMsg time.Time
type entriesMsg struct {
	result *miniflux.EntryResultSet
	offset int
}
type categoriesMsg miniflux.Categories
type feedsMsg miniflux.Feeds
type contentMsg string
type errMsg error
type markReadMsg struct {
	id  int64
	err error
}
type starredMsg struct {
	id  int64
	err error
}

func initialModel(fileContent string, client *miniflux.Client, initialCfg Config) model {
	ti := textinput.New()
	ti.Placeholder = "Search articles..."
	ti.Focus()
	ti.CharLimit = 156
	ti.Width = 30

	urlTi := textinput.New()
	urlTi.Placeholder = "Miniflux URL..."
	urlTi.CharLimit = 200
	urlTi.Width = 50

	m := model{
		wpm:            initialCfg.WPM, // Use WPM from config
		paused:         true,
		rampSpeed:      initialCfg.RampSpeed,
		zenMode:        initialCfg.ZenMode,
		minifluxClient: client,
		searchInput:    ti,
		urlInput:       urlTi,
		cfg:            initialCfg,
	}

	if fileContent != "" {
		m.state = StateReading
		m.content = strings.Fields(fileContent)
	} else if client != nil { // Miniflux client was successfully created (from env or keyring)
		m.state = StateBrowsing
		m.loading = true
	} else { // No file, no client -> must be login
		m.state = StateLogin
		m.urlInput.Focus() // Start with URL input focused
	}

	return m
}

// Keyring Helpers
const (
	minifluxKeyringService = "speedreader-go"
	minifluxKeyringUser    = "miniflux-token"
)

func getMinifluxToken() (string, error) {
	return keyring.Get(minifluxKeyringService, minifluxKeyringUser)
}

func saveMinifluxToken(token string) error {
	return keyring.Set(minifluxKeyringService, minifluxKeyringUser, token)
}

func (m model) Init() tea.Cmd {
	if m.state == StateBrowsing && m.minifluxClient != nil {
		return fetchEntries(m.minifluxClient, "", 0, 0, 0)
	}
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Global keys (except when searching or logging in, where keys go to text input)
		if m.state != StateSearching && m.state != StateLogin {
			switch msg.String() {
			case "ctrl+c", "q":
				return m, tea.Quit
			case "?":
				m.paused = true // Pause if reading
				m.previousState = m.state
				m.state = StateHelp
				return m, nil
			case "esc":
				if m.state == StateHelp {
					// Return to previous state
					m.state = m.previousState
					return m, nil
				}
				if m.state == StateReading && m.minifluxClient != nil {
					m.state = StateBrowsing
					m.paused = true
					return m, nil
				}
				if m.state == StateYouTubeLink && m.minifluxClient != nil {
					m.state = StateBrowsing
					return m, nil
				}
				return m, tea.Quit

			case "c":
				currentTheme = (currentTheme + 1) % len(themes)
				updateTheme(themes[currentTheme])

			case "o":
				// Open in browser
				var url string
				var entryID int64

				if m.currentEntry != nil { // If in reading state (or was reading)
					url = m.currentEntry.URL
					entryID = m.currentEntry.ID
				} else if m.state == StateBrowsing && len(m.entries) > 0 { // If in browsing state
					selectedEntry := m.entries[m.cursor]
					url = selectedEntry.URL
					entryID = selectedEntry.ID
				}

				if url != "" {
					_ = browser.OpenURL(url)
					// If it's a YouTube link and client is available, mark as read
					if m.minifluxClient != nil && entryID != 0 && (strings.Contains(url, "youtube.com/watch?v=") || strings.Contains(url, "youtu.be/")) {
						// This should return a command to mark as read
						return m, markAsRead(m.minifluxClient, entryID)
					}
				}

			case "f":
				// Toggle Starred
				var entryID int64
				if m.currentEntry != nil {
					entryID = m.currentEntry.ID
				} else if m.state == StateBrowsing && len(m.entries) > 0 {
					entryID = m.entries[m.cursor].ID
				}

				if entryID != 0 && m.minifluxClient != nil {
					return m, toggleStarred(m.minifluxClient, entryID)
				}
			}
		}

		// State Specific Handling
		if m.state == StateReading {
			switch msg.String() {
			case " ":
				m.paused = !m.paused
				if !m.paused {
					return m, tick(m.currentDelay())
				}
			case "s":
				m.largeText = !m.largeText
			case "r":
				m.rampSpeed = !m.rampSpeed
			case "z":
				m.zenMode = !m.zenMode
			case "up", "k":
				m.wpm += 50
			case "down", "j":
				if m.wpm > 50 {
					m.wpm -= 50
				}
			case "right":
				m.index += 10
				if m.index >= len(m.content) {
					m.index = len(m.content) - 1
				}
			case "left":
				m.index -= 10
				if m.index < 0 {
					m.index = 0
				}
			case "g":
				m.index = 0
			case "G":
				m.index = len(m.content) - 1
			}
		} else if m.state == StateBrowsing {
			switch msg.String() {
			case "/":
				m.state = StateSearching
				m.searchInput.Focus()
				m.searchInput.SetValue("")
				return m, textinput.Blink
			case "g":
				m.cursor = 0
				m.listOffset = 0
			case "G":
				if len(m.entries) > 0 {
					m.cursor = len(m.entries) - 1

					// Recalculate offset to keep cursor visible (logic similar to 'down' key)
					headerHeight := 3
					visibleHeight := m.height - headerHeight
					// Top indicator space
					if m.cursor > visibleHeight { // If we are jumping far down, top indicator will likely be needed
						visibleHeight--
					}
					visibleHeight-- // Reserve for bottom indicator

					scrollOff := 2
					if visibleHeight < scrollOff+1 {
						visibleHeight = scrollOff + 1
					}

					// Place cursor at bottom with scrollOff
					m.listOffset = m.cursor - (visibleHeight - 1 - scrollOff)
					if m.listOffset < 0 {
						m.listOffset = 0
					}
				}
			case "up", "k":
				if m.cursor > 0 {
					m.cursor--
				}
				if m.cursor < m.listOffset {
					m.listOffset--
				}
			case "down", "j":
				if len(m.entries) > 0 && m.cursor < len(m.entries)-1 {
					m.cursor++
				}
				// Infinite Scroll Trigger
				// If we are within 10 items of the end, and we haven't loaded all items, fetch more
				if !m.fetchingMore && len(m.entries) < m.totalEntries && m.cursor >= len(m.entries)-10 {
					m.fetchingMore = true
					search := ""
					if m.filterYouTube {
						search = "youtube.com/watch?v=|youtu.be/"
					} else if m.searchInput.Value() != "" {
						search = m.searchInput.Value()
					}
					cmd = fetchEntries(m.minifluxClient, search, m.currentCategoryID, m.currentFeedID, len(m.entries))
				}

				// Ensure cursor is visible with scrolloff
				headerHeight := 3
				visibleHeight := m.height - headerHeight

				// Account for potential scroll indicators to be safe
				if m.listOffset > 0 {
					visibleHeight--
				}
				visibleHeight-- // Reserve for bottom indicator

				scrollOff := 2
				if visibleHeight < scrollOff+1 {
					visibleHeight = scrollOff + 1
				}

				// Ensure buffer below cursor
				if m.cursor > m.listOffset+visibleHeight-1-scrollOff {
					m.listOffset = m.cursor - (visibleHeight - 1 - scrollOff)
				}
				return m, cmd

			case "enter":
				if len(m.entries) > 0 {
					selected := m.entries[m.cursor]
					m.loading = true
					m.currentEntry = selected // Store the selected entry

					// Check if it's a YouTube video
					if strings.Contains(selected.URL, "youtube.com/watch?v=") || strings.Contains(selected.URL, "youtu.be/") {
						m.state = StateYouTubeLink
						m.loading = false // No content to fetch
						return m, nil
					}

					return m, fetchContent(selected.Content)
				}
			case "y":
				m.filterYouTube = !m.filterYouTube
				searchTerm := ""
				if m.filterYouTube {
					searchTerm = "youtube.com/watch?v=|youtu.be/" // More specific URL patterns
				}
				m.loading = true
				return m, fetchEntries(m.minifluxClient, searchTerm, m.currentCategoryID, m.currentFeedID, 0)
			case "m":
				// Mark as read manually
				if m.minifluxClient != nil && len(m.entries) > 0 {
					entryID := m.entries[m.cursor].ID
					return m, markAsRead(m.minifluxClient, entryID)
				}
			}
		} else if m.state == StateSearching {
			switch msg.String() {
			case "enter":
				m.state = StateBrowsing
				m.loading = true

				// Reset filters
				m.currentCategoryID = 0
				m.currentFeedID = 0

				searchTerm := m.searchInput.Value()

				if m.searchMode == SearchCategory {
					if len(m.filteredIDs) > 0 && m.searchCursor < len(m.filteredIDs) {
						m.currentCategoryID = m.filteredIDs[m.searchCursor]
						searchTerm = "" // Clear text search when selecting ID
					} else {
						// Invalid selection
						m.state = StateSearching
						m.loading = false
						return m, nil
					}
				} else if m.searchMode == SearchFeed {
					if len(m.filteredIDs) > 0 && m.searchCursor < len(m.filteredIDs) {
						m.currentFeedID = m.filteredIDs[m.searchCursor]
						searchTerm = ""
					} else {
						m.state = StateSearching
						m.loading = false
						return m, nil
					}
				}

				return m, fetchEntries(m.minifluxClient, searchTerm, m.currentCategoryID, m.currentFeedID, 0)

			case "esc":
				m.state = StateBrowsing
				m.searchInput.Blur()
				return m, nil

			case "tab":
				m.searchMode = (m.searchMode + 1) % len(searchModes)
				m.searchInput.SetValue("")
				m.searchCursor = 0
				m.filteredList = nil
				m.filteredIDs = nil

				var cmd tea.Cmd
				if m.searchMode == SearchCategory {
					if len(m.categories) == 0 {
						cmd = fetchCategories(m.minifluxClient)
					} else {
						for _, c := range m.categories {
							m.filteredList = append(m.filteredList, c.Title)
							m.filteredIDs = append(m.filteredIDs, c.ID)
						}
					}
				} else if m.searchMode == SearchFeed {
					if len(m.feeds) == 0 {
						cmd = fetchFeeds(m.minifluxClient)
					} else {
						for _, f := range m.feeds {
							m.filteredList = append(m.filteredList, f.Title)
							m.filteredIDs = append(m.filteredIDs, f.ID)
						}
					}
				}
				return m, cmd

			case "up":
				if m.searchCursor > 0 {
					m.searchCursor--
				}
				return m, nil

			case "down":
				if len(m.filteredList) > 0 && m.searchCursor < len(m.filteredList)-1 {
					m.searchCursor++
				}
				return m, nil
			}

			m.searchInput, cmd = m.searchInput.Update(msg)

			// Post-update filtering
			if m.searchMode == SearchCategory || m.searchMode == SearchFeed {
				term := strings.ToLower(m.searchInput.Value())
				m.filteredList = nil
				m.filteredIDs = nil
				if m.searchMode == SearchCategory {
					for _, c := range m.categories {
						if term == "" || strings.Contains(strings.ToLower(c.Title), term) {
							m.filteredList = append(m.filteredList, c.Title)
							m.filteredIDs = append(m.filteredIDs, c.ID)
						}
					}
				} else {
					for _, f := range m.feeds {
						if term == "" || strings.Contains(strings.ToLower(f.Title), term) {
							m.filteredList = append(m.filteredList, f.Title)
							m.filteredIDs = append(m.filteredIDs, f.ID)
						}
					}
				}
				if m.searchCursor >= len(m.filteredList) {
					m.searchCursor = 0
				}
			}
			return m, cmd
		} else if m.state == StateLogin {
			switch msg.String() {
			case "enter":
				if m.urlInput.Focused() {
					m.urlInput.Blur()
					m.searchInput.Focus() // Use searchInput as token input temporarily
					return m, nil
				} else if m.searchInput.Focused() { // Token is entered
					minifluxURL := m.urlInput.Value()
					minifluxToken := m.searchInput.Value()

					// Save credentials
					if minifluxURL != "" {
						m.cfg.MinifluxURL = minifluxURL // Use the correctly spelled variable
						saveConfig(m.cfg)
					}
					if minifluxToken != "" {
						if err := saveMinifluxToken(minifluxToken); err != nil {
							m.err = fmt.Errorf("failed to save token: %w", err)
						}
					}

					// Try to connect and switch state
					if minifluxURL != "" && minifluxToken != "" {
						m.minifluxClient = miniflux.NewClientWithOptions(
							minifluxURL,
							miniflux.WithAPIKey(minifluxToken),
							miniflux.WithHTTPClient(&http.Client{Timeout: 60 * time.Second}),
						)
						m.state = StateBrowsing
						m.loading = true
						m.urlInput.Blur()
						m.searchInput.Blur()
						return m, fetchEntries(m.minifluxClient, "", 0, 0, 0)
					} else {
						m.err = fmt.Errorf("miniflux URL and Token are required")
						m.urlInput.Focus() // Go back to URL input
					}
					return m, nil
				}
			case "esc": // Quit from login screen
				return m, tea.Quit
			case "tab", "shift+tab":
				if m.urlInput.Focused() {
					m.urlInput.Blur()
					m.searchInput.Focus()
				} else {
					m.searchInput.Blur()
					m.urlInput.Focus()
				}
				return m, nil
			}

			// Handle updates for the focused input
			if m.urlInput.Focused() {
				m.urlInput, cmd = m.urlInput.Update(msg)
			} else if m.searchInput.Focused() {
				m.searchInput, cmd = m.searchInput.Update(msg)
			}
			return m, cmd
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tickMsg:
		if m.state != StateReading || m.paused {
			return m, nil
		}
		if m.index >= len(m.content)-1 {
			m.paused = true

			// Increment stats
			m.sessionArticles++
			m.sessionWords += len(m.content)

			if m.minifluxClient != nil && m.currentEntry != nil {
				return m, markAsRead(m.minifluxClient, m.currentEntry.ID)
			}
			return m, nil
		}
		m.index++
		return m, tick(m.currentDelay())

	case entriesMsg:
		if msg.offset == 0 {
			// Initial load or refresh
			m.entries = msg.result.Entries
			m.totalEntries = msg.result.Total
			m.cursor = 0
			m.listOffset = 0
			m.loading = false
		} else {
			// Append results
			m.entries = append(m.entries, msg.result.Entries...)
			m.totalEntries = msg.result.Total // Update total just in case
		}
		m.fetchingMore = false

	case contentMsg:
		m.content = strings.Fields(string(msg))
		m.state = StateReading
		m.index = 0
		m.paused = true
		m.loading = false

	case errMsg:
		m.err = msg
		m.loading = false
		m.fetchingMore = false

	case markReadMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			// Remove the read entry from the local list
			newEntries := make([]*miniflux.Entry, 0, len(m.entries)-1)
			for _, e := range m.entries {
				if e.ID != msg.id {
					newEntries = append(newEntries, e)
				}
			}
			m.entries = newEntries
			m.totalEntries-- // Decrement total

			// Adjust cursor if necessary
			if m.cursor >= len(m.entries) {
				m.cursor = len(m.entries) - 1
				if m.cursor < 0 {
					m.cursor = 0
				}
			}
		}

	case starredMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			// Toggle locally
			for _, e := range m.entries {
				if e.ID == msg.id {
					e.Starred = !e.Starred
					break
				}
			}
			if m.currentEntry != nil && m.currentEntry.ID == msg.id {
				m.currentEntry.Starred = !m.currentEntry.Starred
			}
		}

	case categoriesMsg:
		m.categories = miniflux.Categories(msg)
		if m.state == StateSearching && m.searchMode == SearchCategory {
			m.filteredList = nil
			m.filteredIDs = nil
			term := strings.ToLower(m.searchInput.Value())
			for _, c := range m.categories {
				if term == "" || strings.Contains(strings.ToLower(c.Title), term) {
					m.filteredList = append(m.filteredList, c.Title)
					m.filteredIDs = append(m.filteredIDs, c.ID)
				}
			}
		}

	case feedsMsg:
		m.feeds = miniflux.Feeds(msg)
		if m.state == StateSearching && m.searchMode == SearchFeed {
			m.filteredList = nil
			m.filteredIDs = nil
			term := strings.ToLower(m.searchInput.Value())
			for _, f := range m.feeds {
				if term == "" || strings.Contains(strings.ToLower(f.Title), term) {
					m.filteredList = append(m.filteredList, f.Title)
					m.filteredIDs = append(m.filteredIDs, f.ID)
				}
			}
		}
	}

	return m, cmd
}

func (m model) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	if m.state == StateBrowsing {
		return m.viewBrowsing()
	} else if m.state == StateSearching {
		return m.viewSearching()
	} else if m.state == StateYouTubeLink {
		return m.viewYouTubeLink()
	} else if m.state == StateLogin {
		return m.viewLogin()
	} else if m.state == StateHelp {
		return m.viewHelp()
	}
	return m.viewReading()
}

func (m model) viewBrowsing() string {
	var sb strings.Builder

	headerText := "Miniflux Unread Entries"
	if m.filterYouTube {
		headerText += " (YouTube Only)"
	}
	header := lipgloss.NewStyle().Bold(true).Render(headerText)
	sb.WriteString(header + "\n\n") // 3 lines used for header

	// Calculate available height for the list
	headerHeight := 3
	visibleHeight := m.height - headerHeight
	if visibleHeight < 0 {
		visibleHeight = 0
	}

	if m.loading && len(m.entries) == 0 {
		sb.WriteString("Loading...")
	} else if m.err != nil {
		sb.WriteString(fmt.Sprintf("Error: %v", m.err))
	} else if len(m.entries) > 0 {
		// Adjust listOffset if entries are fewer than visibleHeight
		if len(m.entries) < m.listOffset+visibleHeight {
			m.listOffset = len(m.entries) - visibleHeight
			if m.listOffset < 0 {
				m.listOffset = 0
			}
		}

		// Render scroll indicator for top
		if m.listOffset > 0 {
			sb.WriteString(normalStyle.Render(strings.Repeat(" ", 15)+"▲ (more above)") + "\n")
			visibleHeight-- // Account for scroll indicator line
		}

		// Reserve space for bottom indicator if needed
		if len(m.entries) > m.listOffset+visibleHeight {
			visibleHeight--
		}

		// Render visible entries
		for i := m.listOffset; i < m.listOffset+visibleHeight && i < len(m.entries); i++ {
			entry := m.entries[i]
			cursor := " "
			style := normalStyle
			if m.cursor == i {
				cursor = ">"
				style = listSelectedStyle
			}

			dateStr := shortDate(entry.Date)
			// Fixed width for date column (max length of "Jan 02 '06" is 10)
			dateWidth := 10
			if len(dateStr) < dateWidth {
				dateStr = dateStr + strings.Repeat(" ", dateWidth-len(dateStr))
			}

			starStr := "  "
			if entry.Starred {
				starStr = "★ "
			}

			// Calculate available width for title
			// Fixed prefix width: Cursor(1) + Space(1) + Date(10) + Space(1) + Star(2) = 15
			prefixWidth := 15
			availableWidth := m.width - prefixWidth - 1 // -1 Buffer
			if availableWidth < 10 {
				availableWidth = 10
			}

			title := strings.ReplaceAll(strings.ReplaceAll(entry.Title, "\n", " "), "\r", "")
			if lipgloss.Width(title) > availableWidth {
				// Truncate
				targetWidth := availableWidth - 1 // -1 for ellipsis
				if targetWidth < 0 {
					targetWidth = 0
				}

				var currentWidth int
				var sbTrunc strings.Builder
				for _, r := range title {
					w := lipgloss.Width(string(r))
					if currentWidth+w > targetWidth {
						break
					}
					sbTrunc.WriteRune(r)
					currentWidth += w
				}
				title = sbTrunc.String() + "…"
			}

			// Use lineStyle (grey) for date
			dateRendered := lineStyle.Render(dateStr)
			starRendered := lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Render(starStr) // Gold color

			sb.WriteString(fmt.Sprintf("%s %s %s%s\n", cursor, dateRendered, starRendered, style.Render(title)))
		}

		// Render scroll indicator for bottom
		if m.listOffset+visibleHeight < len(m.entries) || (len(m.entries) < m.totalEntries) {
			if m.fetchingMore {
				sb.WriteString(normalStyle.Render(strings.Repeat(" ", 15)+"... loading more ...") + "\n")
			} else {
				sb.WriteString(normalStyle.Render(strings.Repeat(" ", 15)+"▼ (more below)") + "\n")
			}
		}
	} else {
		sb.WriteString("No entries found.")
	}

	sb.WriteString("\n\n(/: Search, y: YouTube Filter, m: Mark Read)")

	return appStyle.Width(m.width).Height(m.height).Render(sb.String())
}
func (m model) viewSearching() string {
	var sb strings.Builder

	modeStr := searchModes[m.searchMode]
	header := lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf("Search Articles (%s)", modeStr))
	sb.WriteString(header + "\n\n")

	sb.WriteString(m.searchInput.View())
	sb.WriteString("\n")

	// Render list if applicable
	if m.searchMode == SearchCategory || m.searchMode == SearchFeed {
		sb.WriteString("\n")
		// Calculate available height
		headerHeight := 6 // Header + Input + Spacing
		availableHeight := m.height - headerHeight
		if availableHeight < 5 {
			availableHeight = 5
		}

		start := 0
		end := len(m.filteredList)

		// Simple scrolling logic for search view
		if m.searchCursor >= availableHeight {
			start = m.searchCursor - availableHeight + 1
		}
		if end > start+availableHeight {
			end = start + availableHeight
		}

		for i := start; i < end; i++ {
			cursor := " "
			style := normalStyle
			if i == m.searchCursor {
				cursor = ">"
				style = listSelectedStyle
			}
			sb.WriteString(fmt.Sprintf("%s %s\n", cursor, style.Render(m.filteredList[i])))
		}
	}

	sb.WriteString("\n(Enter to search/select, Tab to change mode, Esc to cancel)")

	return appStyle.Width(m.width).Height(m.height).Render(sb.String())
}

func (m model) viewYouTubeLink() string {
	if m.currentEntry == nil {
		return appStyle.Width(m.width).Height(m.height).Render("No YouTube link selected. (Esc to go back)")
	}

	var sb strings.Builder

	sb.WriteString(lipgloss.NewStyle().Bold(true).Render("YouTube Video Link") + "\n\n")
	sb.WriteString(fmt.Sprintf("Title: %s\n\n", m.currentEntry.Title))
	sb.WriteString(fmt.Sprintf("URL: %s\n\n", m.currentEntry.URL))
	sb.WriteString(lipgloss.NewStyle().Faint(true).Render("(Press Esc to go back to list)"))

	return appStyle.Width(m.width).Height(m.height).Render(sb.String())
}

func (m model) viewLogin() string {
	var sb strings.Builder

	sb.WriteString(lipgloss.NewStyle().Bold(true).Render("Miniflux Login") + "\n\n")

	if m.err != nil {
		sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render(m.err.Error()) + "\n\n")
	}

	sb.WriteString("Miniflux URL:\n")
	sb.WriteString(m.urlInput.View() + "\n\n")

	sb.WriteString("API Token:\n")
	// Mask the token input for security
	tokenInputView := m.searchInput.View()
	if m.searchInput.Focused() {
		sb.WriteString(tokenInputView + "\n\n")
	} else {
		sb.WriteString(strings.Repeat("*", len(m.searchInput.Value())) + "\n\n")
	}

	sb.WriteString("(Enter to switch fields, or submit. Esc to quit)")

	return appStyle.Width(m.width).Height(m.height).Render(sb.String())
}

func (m model) viewHelp() string {
	var sb strings.Builder

	sb.WriteString(lipgloss.NewStyle().Bold(true).Render("Help & Keybindings") + "\n\n")

	keys := []struct {
		Key  string
		Desc string
	}{
		{"Space", "Pause / Resume Reading"},
		{"k / j", "Increase / Decrease WPM"},
		{"Left / Right", "Rewind / Fast Forward (10 words)"},
		{"g / G", "Jump to Start / End"},
		{"s", "Toggle Large Text Size"},
		{"r", "Toggle Speed Ramping"},
		{"z", "Toggle Zen Mode"},
		{"c", "Cycle Themes"},
		{"/", "Search Articles (Miniflux)"},
		{"j / k", "Navigate Article List"},
		{"Enter", "Select Article"},
		{"o", "Open Article in Browser"},
		{"f", "Toggle Starred"},
		{"m", "Mark as Read"},
		{"y", "Filter YouTube Videos"},
		{"Esc", "Back / Quit"},
		{"?", "Show this Help"},
		{"q", "Quit Application"},
	}

	// Calculate max key width for alignment
	maxKeyLen := 0
	for _, k := range keys {
		if len(k.Key) > maxKeyLen {
			maxKeyLen = len(k.Key)
		}
	}

	for _, k := range keys {
		padding := strings.Repeat(" ", maxKeyLen-len(k.Key)+2)
		keyStr := focusStyle.Render(k.Key)
		sb.WriteString(fmt.Sprintf("%s%s%s\n", keyStr, padding, k.Desc))
	}

	sb.WriteString("\n" + lipgloss.NewStyle().Faint(true).Render("Press Esc to close."))

	return appStyle.Width(m.width).Height(m.height).Render(sb.String())
}

func (m model) viewReading() string {
	if m.width == 0 {
		return "File is empty."
	}

	if m.index >= len(m.content) {
		m.index = len(m.content) - 1
	}

	// Helper for full-width background lines
	blankLine := normalStyle.Render(strings.Repeat(" ", m.width))

	// 1. Prepare Content Line (Left + Focus + Right)
	word := m.content[m.index]
	left, focus, right := calculateORP(word)

	if m.largeText {
		left = toFullWidth(left)
		focus = toFullWidth(focus)
		right = toFullWidth(right)
	}

	// ORP Alignment Logic
	centerX := m.width / 2

	leftStr := normalStyle.Render(left)
	focusStr := focusStyle.Render(focus)
	rightStr := normalStyle.Render(right)

	// Left Padding
	leftLen := lipgloss.Width(left) // Width of the characters
	padLen := centerX - leftLen
	if padLen < 0 {
		padLen = 0
	}
	leftPadding := normalStyle.Render(strings.Repeat(" ", padLen))

	// Right Padding
	currentContentWidth := lipgloss.Width(leftPadding) + lipgloss.Width(leftStr) + lipgloss.Width(focusStr) + lipgloss.Width(rightStr)
	rightPadLen := m.width - currentContentWidth
	if rightPadLen < 0 {
		rightPadLen = 0
	}
	rightPadding := normalStyle.Render(strings.Repeat(" ", rightPadLen))

	contentLine := leftPadding + leftStr + focusStr + rightStr + rightPadding

	// 2. Prepare Separators & Gaps
	separator := lineStyle.Render(strings.Repeat("─", m.width))

	// 3. Prepare HUD
	progressBar := m.renderProgressBar()
	timeRemaining := m.renderTimeRemaining()
	wpmStr := fmt.Sprintf("WPM: %d", m.wpm)
	status := "PLAYING"
	if m.paused {
		status = "PAUSED (Press Space)"
	}

	rampStatus := "OFF"
	if m.rampSpeed {
		rampStatus = "ON"
	}

	hudText := fmt.Sprintf("%s | %s\n%s\n%s | Size: s | Color: c | Ramp: r (%s) | Zen: z", wpmStr, timeRemaining, progressBar, status, rampStatus)

	// Add navigation hint for Miniflux users
	if m.minifluxClient != nil {
		hudText += " | Esc: Back | o: Open | f: Star"
	}
	if m.currentEntry != nil {
		hudText = fmt.Sprintf("%s\nTitle: %s", hudText, m.currentEntry.Title)
	}

	var hudRendered string
	var hudHeight int

	if !m.zenMode {
		hudRendered = hudStyle.Width(m.width).Render(hudText)
		hudHeight = lipgloss.Height(hudRendered)
	} else {
		hudHeight = 0
	}

	// 4. Vertical Layout Calculation
	totalHeight := m.height
	mainHeight := totalHeight - hudHeight
	if mainHeight < 0 {
		mainHeight = 0
	}

	showSeparators := m.height > 10
	verticalGap := 1

	contentBlockHeight := 1
	if showSeparators && !m.zenMode {
		contentBlockHeight = 1 + (1+verticalGap)*2
	}

	topPadding := (mainHeight - contentBlockHeight) / 2
	if topPadding < 0 {
		topPadding = 0
	}

	bottomPadding := mainHeight - contentBlockHeight - topPadding
	if bottomPadding < 0 {
		bottomPadding = 0
	}

	// 5. Build the View
	var sb strings.Builder

	// Top Fill
	for i := 0; i < topPadding; i++ {
		sb.WriteString(blankLine + "\n")
	}

	// Content Block
	if showSeparators && !m.zenMode {
		sb.WriteString(separator + "\n")
		for i := 0; i < verticalGap; i++ {
			sb.WriteString(blankLine + "\n")
		}
	}

	sb.WriteString(contentLine + "\n")

	if showSeparators && !m.zenMode {
		for i := 0; i < verticalGap; i++ {
			sb.WriteString(blankLine + "\n")
		}
		sb.WriteString(separator + "\n")
	}

	// Bottom Fill
	for i := 0; i < bottomPadding; i++ {
		sb.WriteString(blankLine + "\n")
	}

	// HUD
	if !m.zenMode {
		sb.WriteString(hudRendered)
	}

	return sb.String()
}

// Commands
func fetchEntries(client *miniflux.Client, search string, categoryID int64, feedID int64, offset int) tea.Cmd {
	return func() tea.Msg {
		filter := &miniflux.Filter{Status: "unread", Limit: 50, Order: "published_at", Direction: "desc", Offset: offset}
		if search != "" {
			filter.Search = search
		}
		if categoryID != 0 {
			filter.CategoryID = categoryID
		}
		if feedID != 0 {
			filter.FeedID = feedID
		}
		entries, err := client.Entries(filter)
		if err != nil {
			return errMsg(err)
		}
		return entriesMsg{result: entries, offset: offset}
	}
}

func fetchCategories(client *miniflux.Client) tea.Cmd {
	return func() tea.Msg {
		categories, err := client.Categories()
		if err != nil {
			return errMsg(err)
		}
		return categoriesMsg(categories)
	}
}

func fetchFeeds(client *miniflux.Client) tea.Cmd {
	return func() tea.Msg {
		feeds, err := client.Feeds()
		if err != nil {
			return errMsg(err)
		}
		return feedsMsg(feeds)
	}
}

func fetchContent(htmlContent string) tea.Cmd {
	return func() tea.Msg {
		text := html2text.HTML2Text(htmlContent)
		return contentMsg(text)
	}
}

func markAsRead(client *miniflux.Client, entryID int64) tea.Cmd {
	return func() tea.Msg {
		err := client.UpdateEntries([]int64{entryID}, "read")
		return markReadMsg{id: entryID, err: err}
	}
}

func toggleStarred(client *miniflux.Client, entryID int64) tea.Cmd {
	return func() tea.Msg {
		err := client.ToggleStarred(entryID)
		return starredMsg{id: entryID, err: err}
	}
}

// Logic Helpers

func (m model) currentDelay() time.Duration {
	baseDelay := 60.0 / float64(m.wpm)

	word := m.content[m.index]

	// Complexity Ramping
	if m.rampSpeed {
		length := len(word)
		if length > 12 {
			baseDelay *= 1.5
		} else if length > 8 {
			baseDelay *= 1.2
		}
	}

	// Basic Punctuation detection
	if strings.HasSuffix(word, ".") || strings.HasSuffix(word, "!") || strings.HasSuffix(word, "?") {
		baseDelay *= 2.0
	} else if strings.HasSuffix(word, ",") || strings.HasSuffix(word, ";") {
		baseDelay *= 1.5
	}

	// Convert seconds to duration
	return time.Duration(baseDelay * float64(time.Second))
}

func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func calculateORP(word string) (string, string, string) {
	runes := []rune(word)
	n := len(runes)
	var focusIdx int

	// Simple heuristic for ORP (Optimal Recognition Point)
	// Roughly 35% into the word, slightly adjusted for length
	if n == 1 {
		focusIdx = 0
	} else if n >= 2 && n <= 5 {
		focusIdx = 1
	} else if n >= 6 && n <= 9 {
		focusIdx = 2
	} else if n >= 10 && n <= 13 {
		focusIdx = 3
	} else {
		focusIdx = 4
	}

	// Safety check
	if focusIdx >= n {
		focusIdx = n - 1
	}

	if n == 0 {
		return "", "", ""
	}

	left := string(runes[:focusIdx])
	focus := string(runes[focusIdx])
	right := string(runes[focusIdx+1:])

	return left, focus, right
}

func (m model) renderProgressBar() string {
	total := len(m.content)
	if total == 0 {
		return ""
	}
	percent := float64(m.index) / float64(total)
	barWidth := 40
	filled := int(percent * float64(barWidth))

	bar := "[" + strings.Repeat("=", filled) + strings.Repeat("-", barWidth-filled) + "]"
	return fmt.Sprintf("%s %d%%", bar, int(percent*100))
}

func (m model) renderTimeRemaining() string {
	wordsLeft := len(m.content) - m.index
	minutes := float64(wordsLeft) / float64(m.wpm)
	seconds := int(minutes * 60)

	return fmt.Sprintf("Time Remaining: %02d:%02d", seconds/60, seconds%60)
}

// Config
type Config struct {
	WPM           int    `json:"wpm"`
	ThemeIndex    int    `json:"theme_index"`
	RampSpeed     bool   `json:"ramp_speed"`
	ZenMode       bool   `json:"zen_mode"`
	TotalArticles int    `json:"total_articles"`
	TotalWords    int    `json:"total_words"`
	MinifluxURL   string `json:"miniflux_url"`
}

func getConfigPath() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "speedreader.json"
	}
	return filepath.Join(configDir, "speedreader.json")
}

func loadConfig() Config {
	path := getConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{WPM: 300, ThemeIndex: 0, TotalArticles: 0, TotalWords: 0, MinifluxURL: ""}
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{WPM: 300, ThemeIndex: 0, TotalArticles: 0, TotalWords: 0, MinifluxURL: ""}
	}
	return cfg
}

func saveConfig(cfg Config) {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err == nil {
		os.WriteFile(getConfigPath(), data, 0644)
	}
}

func main() {
	var fileContent string
	var client *miniflux.Client
	var minifluxURL string
	var minifluxToken string

	// 1. Check for file argument
	if len(os.Args) > 1 {
		fileName := os.Args[1]
		content, err := os.ReadFile(fileName)
		if err != nil {
			fmt.Printf("Error reading file: %v\n", err)
			os.Exit(1)
		}
		fileContent = string(content)
	}

	// Load Config (for MinifluxURL)
	cfg := loadConfig()
	currentTheme = cfg.ThemeIndex
	if currentTheme >= len(themes) {
		currentTheme = 0
	}

	updateTheme(themes[currentTheme]) // Apply initial theme

	initialWPM := cfg.WPM
	if initialWPM <= 0 {
		initialWPM = 300
	}

	// 2. Try to get Miniflux credentials
	if fileContent == "" { // Only try Miniflux if no local file is given
		// Try from environment variables first
		minifluxURL = os.Getenv("MINIFLUX_URL")
		minifluxToken = os.Getenv("MINIFLUX_API_TOKEN")

		// If not in env, try from config (for URL) and keyring (for token)
		if minifluxURL == "" {
			minifluxURL = cfg.MinifluxURL
		}
		if minifluxToken == "" && minifluxURL != "" { // Only try keyring if URL is present
			var err error
			minifluxToken, err = getMinifluxToken()
			if err != nil {
				// Log error but don't exit, will go to login state
				fmt.Fprintf(os.Stderr, "Error getting token from keyring: %v\n", err)
			}
		}

		if minifluxURL != "" && minifluxToken != "" {
			client = miniflux.NewClientWithOptions(
				minifluxURL,
				miniflux.WithAPIKey(minifluxToken),
				miniflux.WithHTTPClient(&http.Client{Timeout: 60 * time.Second}),
			)
		}
	}

	m := initialModel(fileContent, client, cfg)

	// If starting in login state, pre-fill from loaded config
	if m.state == StateLogin {
		m.urlInput.SetValue(minifluxURL)
		// Token is not pre-filled into text input for security
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		fmt.Printf("Alas, there's been an error: %v", err)
		os.Exit(1)
	}

	if m, ok := finalModel.(model); ok {
		// Update cumulative stats and save
		m.cfg.WPM = m.wpm
		m.cfg.ThemeIndex = currentTheme
		m.cfg.RampSpeed = m.rampSpeed
		m.cfg.ZenMode = m.zenMode
		m.cfg.TotalArticles += m.sessionArticles
		m.cfg.TotalWords += m.sessionWords
		// MinifluxURL is updated earlier if in login state (m.cfg.MinifluxURL)
		saveConfig(m.cfg)

		// Print Session Summary
		fmt.Println("\n--- Session Summary ---")
		fmt.Printf("Articles Read: %d\n", m.sessionArticles)
		fmt.Printf("Words Read:    %d\n", m.sessionWords)
		fmt.Println("-----------------------")
		fmt.Printf("Total All-Time: %d articles, %d words\n", m.cfg.TotalArticles, m.cfg.TotalWords)
	}
}
func toFullWidth(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r == ' ':
			sb.WriteRune('\u3000') // Ideographic Space
		case r >= '!' && r <= '~':
			sb.WriteRune(r + 0xFEE0)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func updateTheme(bg lipgloss.Color) {
	// Determine foreground color based on background brightness
	fgColor := lipgloss.Color("255")  // White text default
	hudColor := lipgloss.Color("240") // Grey default

	// Special handling for default terminal background
	if bg == lipgloss.Color("") {
		focusStyle = focusStyle.Background(lipgloss.NoColor{})
		normalStyle = normalStyle.Background(lipgloss.NoColor{})
		hudStyle = hudStyle.Background(lipgloss.NoColor{})
		lineStyle = lineStyle.Background(lipgloss.NoColor{})
		appStyle = appStyle.Background(lipgloss.NoColor{})

		// When background is default, we want our text to be readable on whatever the user has.
		// For consistency, let's keep text bright (white) unless it's a light theme.
		// So the fgColor logic still needs to run.
		// We'll reset all backgrounds to NoColor and then let fg logic apply.
	} else {
		// Normal theme logic
		// Very rough heuristic for light themes
		if bg == lipgloss.Color("#ffffff") || bg == lipgloss.Color("#fbf1c7") {
			fgColor = lipgloss.Color("0")    // Black text
			hudColor = lipgloss.Color("238") // Darker grey for HUD
		}

		focusStyle = focusStyle.Background(bg)
		normalStyle = normalStyle.Background(bg)
		hudStyle = hudStyle.Background(bg)
		lineStyle = lineStyle.Background(bg)
		appStyle = appStyle.Background(bg)
	}

	// Apply foreground colors after background is set
	focusStyle = focusStyle.Foreground(lipgloss.Color("196")) // Red remains red
	normalStyle = normalStyle.Foreground(fgColor)
	hudStyle = hudStyle.Foreground(hudColor)
	lineStyle = lineStyle.Foreground(lipgloss.Color("238")) // Dark Grey remains dark grey
	appStyle = appStyle.Foreground(fgColor)
}

func shortDate(t time.Time) string {
	now := time.Now()
	if t.Year() != now.Year() {
		return t.Format("Jan 02 '06")
	}
	if t.YearDay() != now.YearDay() {
		return t.Format("Jan 02")
	}
	return t.Format("15:04")
}
