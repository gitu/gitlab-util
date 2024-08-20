package glui

import (
	"bytes"
	"fmt"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/dustin/go-humanize"
	"github.com/gitu/gitlab-util/pkg/ggl"
	"github.com/icza/gox/osx"
	"github.com/xanzy/go-gitlab"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

var baseStyle = lipgloss.NewStyle().
	BorderStyle(lipgloss.NormalBorder()).
	BorderForeground(lipgloss.Color("240"))

var (
	titleStyle = func() lipgloss.Style {
		b := lipgloss.RoundedBorder()
		b.Right = "├"
		return lipgloss.NewStyle().BorderStyle(b).Padding(0, 1)
	}()

	infoStyle = func() lipgloss.Style {
		b := lipgloss.RoundedBorder()
		b.Left = "┤"
		return titleStyle.BorderStyle(b)
	}()
)

type model struct {
	table         table.Model
	gl            *gitlab.Client
	mergeRequests []mergeRequest
	spinner       spinner.Model
	loading       string
	mrm           *ggl.MergeRequestManager
	rowmap        map[string]int
	diff          []*gitlab.MergeRequestDiff
	diffView      viewport.Model
	ready         bool
	diffId        int
	diffTitle     string
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.InitialFetch())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case mergeRequests:
		if msg.err == nil {
			m.mergeRequests = msg.requests
			m.rowmap = make(map[string]int)
			rows := make([]table.Row, len(msg.requests))
			for i, r := range msg.requests {
				m.rowmap[r.HumanId] = r.Id
				lastAction := humanize.RelTime(r.LastAction, time.Now(), "ago", "from now")
				nextAction := humanize.RelTime(r.NextAction, time.Now(), "ago", "from now")
				if r.Info == "" {
					lastAction = ""
				}
				if !r.Active {
					nextAction = ""
				}
				rows[i] = table.Row{
					r.HumanId,
					r.Title,
					humanize.RelTime(r.LastUpdate, time.Now(), "ago", "from now"),
					r.MergeStatus,
					r.Info,
					lastAction,
					nextAction,
				}
			}
			m.table.SetRows(rows)
			m.loading = ""
		}
		if !msg.oneShot {
			return m, m.mergeRequestor()
		}
		return m, nil
	case []*gitlab.MergeRequestDiff:
		m.loading = ""
		m.diff = msg
		m.diffView.SetContent(ggl.RenderDiffString(msg))
		m.diffView, cmd = m.diffView.Update(msg)
		return m, cmd
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			if m.table.Focused() {
				m.table.Blur()
			} else {
				m.table.Focus()
			}
			break
		case "ctrl+c":
			return m, tea.Quit
		}
		if m.diff != nil {
			switch msg.String() {
			case "q":
				m.diff = nil
				break
			case "m":
				m.loading = "Approving & Merging " + m.diffTitle
				return m, m.approveAndMergeMergeRequest(m.diffId, m.diff)
			}

			break
		}
		switch msg.String() {
		case "q":
			return m, tea.Quit
		case "d", "enter":
			m.loading = "Diff"
			m.diffId = m.rowmap[m.table.SelectedRow()[0]]
			m.diffTitle = m.table.SelectedRow()[0] + " | " + m.table.SelectedRow()[1]
			return m, m.loadDiff(m.diffId)
		case "o":
			id := m.rowmap[m.table.SelectedRow()[0]]
			request, err := m.mrm.GetMergeRequest(id)
			if err != nil {
				return m, nil
			}
			url := request.WebURL
			_ = osx.OpenDefault(url)
			return m, nil
		case "c":
			id := m.rowmap[m.table.SelectedRow()[0]]
			return m, m.clearMerge(id)
		case "r":
			return m, m.fetchMergeRequestsForced
		}
		m.table, cmd = m.table.Update(msg)
		return m, cmd
	case tea.WindowSizeMsg:
		headerHeight := lipgloss.Height(m.headerView())
		footerHeight := lipgloss.Height(m.footerView())
		verticalMarginHeight := headerHeight + footerHeight

		m.table.SetHeight(msg.Height - 6)
		m.table.SetWidth(msg.Width - 5)

		if !m.ready {
			// Since this program is using the full size of the viewport we
			// need to wait until we've received the window dimensions before
			// we can initialize the viewport. The initial dimensions come in
			// quickly, though asynchronously, which is why we wait for them
			// here.
			m.diffView = viewport.New(msg.Width, msg.Height-verticalMarginHeight)
			m.diffView.YPosition = headerHeight
			m.ready = true

			// This is only necessary for high performance rendering, which in
			// most cases you won't need.
			//
			// Render the viewport one line below the header.
			m.diffView.YPosition = headerHeight + 1
		} else {
			m.diffView.Width = msg.Width
			m.diffView.Height = msg.Height - verticalMarginHeight
		}
		cmds = append(cmds, viewport.Sync(m.diffView))
		break
	case approvalState:
		m.loading = ""
		m.diff = nil
		return m, m.fetchMergeRequests
	case spinner.TickMsg:
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	if m.diff != nil {
		m.diffView, cmd = m.diffView.Update(msg)
	}
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

func (m model) View() string {
	if !m.ready {
		return "\n  Initializing..."
	}
	if m.loading != "" {
		return m.spinner.View() + " Loading " + m.loading + "...\n"
	}
	if m.diff != nil {
		return fmt.Sprintf("%s\n%s\n%s", m.headerView(), m.diffView.View(), m.footerView())
	}
	return baseStyle.Render(m.table.View()) + "\n"
}

func (m model) headerView() string {
	title := titleStyle.Render(m.diffTitle)
	line := strings.Repeat("─", max(0, m.diffView.Width-lipgloss.Width(title)))
	return lipgloss.JoinHorizontal(lipgloss.Center, title, line)
}

func (m model) footerView() string {
	info := infoStyle.Render(fmt.Sprintf("%3.f%%", m.diffView.ScrollPercent()*100))
	line := strings.Repeat("─", max(0, m.diffView.Width-lipgloss.Width(info)))
	return lipgloss.JoinHorizontal(lipgloss.Center, line, info)
}

func (m model) mergeRequestor() tea.Cmd {
	return tea.Every(1*time.Second, func(time.Time) tea.Msg {
		return m.fetchMergeRequestsVariable(false, false)
	})
}
func (m model) fetchMergeRequests() tea.Msg {
	return m.fetchMergeRequestsVariable(true, false)
}

func (m model) fetchMergeRequestsForced() tea.Msg {
	return m.fetchMergeRequestsVariable(true, true)
}

func (m model) fetchMergeRequestsVariable(oneshot bool, force bool) tea.Msg {
	if oneshot || force {
		start := time.Now()
		defer func() {
			log.Printf("Fetched merge requests in %v", time.Since(start))
		}()
	}

	requests, err := m.mrm.GetOrFetchMergeRequests(force)
	if err != nil {
		log.Println("Error fetching merge requests", err)
		return mergeRequests{err: err}
	}

	var mrs mergeRequests
	for _, r := range requests {
		mrs.requests = append(mrs.requests, m.mapMergeRequest(&r))
	}
	mrs.oneShot = oneshot
	return mrs
}

type mergeRequests struct {
	requests []mergeRequest
	oneShot  bool
	err      error
}

type mergeRequest struct {
	MergeStatus string
	HumanId     string
	Id          int
	Title       string
	Active      bool
	Info        string
	LastUpdate  time.Time
	LastAction  time.Time
	NextAction  time.Time
}

func (m model) mapMergeRequest(r *ggl.MergeRequestInfo) mergeRequest {
	p, err := m.mrm.GetProject(r.ProjectID)
	if err != nil {
		log.Println("Error fetching project", r.ProjectID, err)
		return mergeRequest{}
	}
	var lastUpdate time.Time
	if r.UpdatedAt != nil {
		lastUpdate = *r.UpdatedAt
	}
	return mergeRequest{
		Id:          r.ID,
		HumanId:     p.Name + "!" + strconv.Itoa(r.IID),
		Title:       r.Title,
		MergeStatus: r.DetailedMergeStatus,
		Active:      r.Target.Active,
		Info:        r.Target.Info,
		LastAction:  r.Target.Latest,
		NextAction:  r.Target.Next,
		LastUpdate:  lastUpdate,
	}
}

func (m model) InitialFetch() tea.Cmd {
	return func() tea.Msg {
		return m.fetchMergeRequestsVariable(false, false)
	}
}

func (m model) loadDiff(id int) tea.Cmd {
	return func() tea.Msg {
		diff, err := m.mrm.PullDiff(id)
		if err != nil {
			log.Println("Error fetching diff", err)
			return err
		}
		log.Println("Diff", diff)
		return diff
	}
}

type approvalState struct {
}

func (m model) approveAndMergeMergeRequest(id int, diff []*gitlab.MergeRequestDiff) tea.Cmd {
	return func() tea.Msg {
		err := m.mrm.ApproveAndMergeMergeRequest(id, diff)
		if err != nil {
			log.Println("Error approving and merging", err)
			return err
		}
		return approvalState{}
	}
}

func (m model) clearMerge(id int) tea.Cmd {
	return func() tea.Msg {
		err := m.mrm.ClearMerge(id)
		if err != nil {
			log.Println("Error clearing merge", err)
			return err
		}
		return m.fetchMergeRequestsForced()
	}
}

func AutoMerge(author, reviewer, logFile string) error {
	buf := bytes.NewBuffer(nil)
	log.SetOutput(buf)
	if logFile != "" {
		f, err := tea.LogToFile(logFile, "")
		if err != nil {
			fmt.Println("fatal:", err)
			os.Exit(1)
		}
		defer f.Close()
	}

	columns := []table.Column{
		{Title: "#", Width: 20},
		{Title: "Title", Width: 80},
		{Title: "Updated", Width: 20},
		{Title: "State", Width: 15},
		{Title: "Action Info", Width: 40},
		{Title: "Last Action", Width: 20},
		{Title: "Next Try", Width: 20},
	}

	rows := []table.Row{}

	t := table.New(
		table.WithColumns(columns),
		table.WithRows(rows),
		table.WithFocused(true),
		table.WithHeight(7),
	)

	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("240")).
		BorderBottom(true).
		Bold(false)
	s.Selected = s.Selected.
		Foreground(lipgloss.Color("229")).
		Background(lipgloss.Color("57")).
		Bold(false)
	t.SetStyles(s)

	gl, err := ggl.GetDefaultClient()
	if err != nil {
		fmt.Println("Error getting client", err)
		return err
	}

	badger, err := ggl.GetDefaultDb()
	if err != nil {
		fmt.Println("Error getting badger", err)
		return err
	}

	m := model{
		table:   t,
		gl:      gl,
		mrm:     ggl.NewMergeRequestManager(badger, gl).Reviewer(reviewer).Author(author).Start(),
		spinner: spinner.New(spinner.WithSpinner(spinner.Moon)),
		loading: "Merge Requests"}
	p := tea.NewProgram(
		m,
		tea.WithAltScreen(),       // use the full size of the terminal in its "alternate screen buffer"
		tea.WithMouseCellMotion(), // turn on mouse support so we can track the mouse wheel
	)
	if _, err := p.Run(); err != nil {
		return err
	}
	return nil
}
