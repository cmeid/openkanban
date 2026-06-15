package ui

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
)

func (m *Model) View() string {
	if m.width == 0 || m.height == 0 {
		loadingStyle := lipgloss.NewStyle().
			Foreground(m.colors.primary).
			Bold(true)
		return lipgloss.Place(
			80, 24,
			lipgloss.Center, lipgloss.Center,
			loadingStyle.Render("◈ Initializing..."),
		)
	}

	if m.mode == ModeShuttingDown {
		return m.renderShuttingDown()
	}

	if m.mode == ModeSpawning {
		return m.renderSpawning()
	}

	if m.mode == ModeAgentView && m.focusedPane != "" {
		return m.renderAgentView()
	}

	var b strings.Builder

	b.WriteString(m.renderHeader())
	b.WriteString("\n")

	sidebar := m.renderSidebar()
	board := m.renderBoard()
	if sidebar != "" {
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, sidebar, board))
	} else {
		b.WriteString(board)
	}

	if m.showHelp {
		return m.renderWithOverlay(m.renderHelp())
	}
	if m.showChoice {
		return m.renderWithOverlay(m.renderChoiceDialog())
	}
	if m.mode == ModeConfirmExit {
		return m.renderWithOverlay(m.renderConfirmExitDialog())
	}
	if m.showConfirm {
		return m.renderWithOverlay(m.renderConfirmDialog())
	}
	if m.mode == ModeCreateTicket || m.mode == ModeEditTicket {
		return m.renderWithOverlay(m.renderTicketForm())
	}
	if m.mode == ModeSettings {
		return m.renderWithOverlay(m.renderSettingsView())
	}
	if m.mode == ModeCreateProject {
		return m.renderWithOverlay(m.renderCreateProjectForm())
	}

	b.WriteString("\n")
	b.WriteString(m.renderStatusBar())

	return b.String()
}

func (m *Model) renderHeader() string {
	logo := lipgloss.NewStyle().
		Foreground(m.colors.primary).
		Bold(true).
		Render("◈ OpenKanban")

	var filterSection string
	if m.mode == ModeFilter {
		filterSection = m.renderFilterInput()
	} else if m.filterQuery != "" || len(m.filterProjectIDs) > 0 {
		filterSection = m.renderActiveFilter()
	} else {
		filterSection = m.renderFilterHint()
	}

	projectCount := len(m.globalStore.Projects())
	ticketCount := m.globalStore.Count()
	visibleCount := m.countVisibleTickets()
	var stats string
	if m.filterQuery != "" || len(m.filterProjectIDs) > 0 {
		stats = m.dimStyle().Render(fmt.Sprintf("showing %d of %d", visibleCount, ticketCount))
	} else {
		stats = m.dimStyle().Render(fmt.Sprintf("%d projects, %d tickets", projectCount, ticketCount))
	}

	left := lipgloss.JoinHorizontal(lipgloss.Center, logo, "  ", filterSection, "  ", stats)

	workingCount, waitingCount, idleCount := 0, 0, 0
	for ticketID, pane := range m.panes {
		if !pane.Running() {
			continue
		}
		ticket, _ := m.globalStore.Get(ticketID)
		if ticket == nil {
			continue
		}

		switch ticket.AgentStatus {
		case board.AgentWorking:
			workingCount++
		case board.AgentWaiting:
			waitingCount++
		case board.AgentIdle:
			idleCount++
		}
	}

	var statusText string
	var bgColor, fgColor lipgloss.Color
	fgColor = m.colors.base

	if waitingCount > 0 {
		bgColor = m.colors.secondary
		statusText = fmt.Sprintf("◐ %d waiting", waitingCount)
		if workingCount > 0 {
			statusText = fmt.Sprintf("◐ %d waiting, %d working", waitingCount, workingCount)
		}
	} else if workingCount > 0 {
		bgColor = m.colors.warning
		statusText = fmt.Sprintf("%s %d working", m.spinner.View(), workingCount)
	} else if idleCount > 0 {
		bgColor = m.colors.primary
		statusText = fmt.Sprintf("◆ %d idle", idleCount)
	} else {
		bgColor = m.colors.surface
		fgColor = m.colors.muted
		statusText = "○ 0 sessions"
	}

	activity := lipgloss.NewStyle().
		Foreground(fgColor).
		Background(bgColor).
		Bold(true).
		Padding(0, 1).
		Render(statusText)

	helpStyle := lipgloss.NewStyle().Foreground(m.colors.muted)
	help := helpStyle.Render("? help  q quit")

	right := lipgloss.JoinHorizontal(lipgloss.Center, activity, "  ", help)

	spacing := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	spacing = max(spacing, 0)

	header := lipgloss.JoinHorizontal(lipgloss.Center, left, strings.Repeat(" ", spacing), right)

	return lipgloss.NewStyle().
		PaddingTop(1).
		PaddingBottom(1).
		BorderBottom(true).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(m.colors.surface).
		Width(m.width).
		Render(header)
}

func (m *Model) renderBoard() string {
	columnWidth := m.calcColumnWidth()
	visibleCols := m.visibleColumnCount(columnWidth)

	startCol := m.scrollOffset
	endCol := min(startCol+visibleCols, len(m.columns))

	numVisible := endCol - startCol
	baseWidth, remainder := m.distributeWidth(numVisible)

	var columns []string

	if startCol > 0 {
		indicator := lipgloss.NewStyle().
			Foreground(m.colors.muted).
			Background(m.colors.surface).
			Padding(0, 1).
			Render(fmt.Sprintf("◀ %d", startCol))
		columns = append(columns, indicator)
	}

	for i := startCol; i < endCol; i++ {
		col := m.columns[i]
		isActive := i == m.activeColumn && !m.sidebarFocused
		isLast := i == endCol-1
		isDragTarget := m.dragging && i == m.dragTargetColumn && i != m.dragSourceColumn
		isHovered := i == m.hoverColumn && !m.dragging

		colWidth := baseWidth
		if i-startCol < remainder {
			colWidth++
		}

		ticketOffset := 0
		if i < len(m.columnOffsets) {
			ticketOffset = m.columnOffsets[i]
		}

		columns = append(columns, m.renderColumn(i, col, m.columnTickets[i], isActive, isDragTarget, isHovered, colWidth, isLast, ticketOffset))
	}

	if endCol < len(m.columns) {
		remaining := len(m.columns) - endCol
		indicator := lipgloss.NewStyle().
			Foreground(m.colors.muted).
			Background(m.colors.surface).
			Padding(0, 1).
			Render(fmt.Sprintf("%d ▶", remaining))
		columns = append(columns, indicator)
	}

	return lipgloss.JoinHorizontal(lipgloss.Top, columns...)
}

func (m *Model) renderColumn(colIdx int, col board.Column, tickets []*board.Ticket, isActive, isDragTarget, isHovered bool, width int, isLast bool, ticketOffset int) string {
	headerColor := m.columnColor(col.Status)

	columnIcons := map[board.TicketStatus]string{
		board.StatusBacklog:    "📋",
		board.StatusInProgress: "⚡",
		board.StatusInReview:   "👀",
		board.StatusDone:       "✅",
	}
	icon := columnIcons[col.Status]
	if icon == "" {
		icon = "○"
	}
	if isActive {
		icon = "▸ " + icon
	}

	headerText := fmt.Sprintf("%s %s", icon, col.Name)

	countStyle := lipgloss.NewStyle().Foreground(m.colors.muted)
	countText := fmt.Sprintf("(%d)", len(tickets))
	if col.Limit > 0 {
		countText = fmt.Sprintf("(%d/%d)", len(tickets), col.Limit)
		if len(tickets) >= col.Limit {
			countStyle = lipgloss.NewStyle().
				Foreground(m.colors.base).
				Background(m.colors.err).
				Padding(0, 1)
		}
	}

	header := lipgloss.NewStyle().
		Foreground(headerColor).
		Bold(true).
		Render(headerText)

	count := countStyle.Render(" " + countText)

	// Clamp the column header to a single row. width-2 matches the column's
	// effective content width after Padding(0,1) on the column style block
	// below (see the lipgloss.NewStyle().Border(border)...Width(width).Padding(0,1)
	// chain near the end of this function); keep these widths in lockstep if
	// the column padding ever changes. Without this clamp, a long column name
	// at small widths wraps to 2 rows and breaks the columnHeaderHeight = 3
	// invariant (top border + header + blank separator).
	headerLine := lipgloss.NewStyle().MaxHeight(1).Width(width - 2).Render(header + count)

	// Render and measure every ticket so we can both (1) cache per-ticket
	// heights for hitTestTicket / ensureTicketVisible (which need the FULL
	// mapping, not just the visible window) and (2) pack as many visible
	// tickets as fit within columnContentHeight, accounting for the
	// ▲/▼ overflow indicators.
	//
	// Empirically measured (see empirical_test.go):
	//   - short-title card height = 8 rows
	//   - long-title card height  = 9 rows (title wraps to 2 lines)
	//   - lipgloss.Height(strings.Join(views, "\n")) == sum of individual
	//     heights — MarginBottom(1) does NOT overlap the join separator
	//     here, so the per-ticket cost is just h (no -1 adjustment).
	heights := make([]int, len(tickets))
	rendered := make([]string, len(tickets))
	for i, ticket := range tickets {
		isSelected := isActive && i == m.activeTicket
		isTicketHovered := isHovered && i == m.hoverTicket
		v := m.renderTicket(ticket, isSelected, isTicketHovered, width-4, headerColor, i+1, len(tickets))
		rendered[i] = v
		heights[i] = lipgloss.Height(v)
	}
	if colIdx >= 0 && colIdx < len(m.columnTicketHeights) {
		m.columnTicketHeights[colIdx] = heights
	}

	budget := m.columnContentHeight()
	if ticketOffset > 0 {
		budget -= 1 // ▲ N more indicator above
	}

	endIdx := ticketOffset
	for i := ticketOffset; i < len(tickets); i++ {
		cost := heights[i]
		// Reserve a row for the ▼ N more indicator if anything remains
		// after this card.
		indicatorReserve := 0
		if i < len(tickets)-1 {
			indicatorReserve = 1
		}
		if cost+indicatorReserve > budget {
			break
		}
		budget -= cost
		endIdx = i + 1
	}
	// At minimum render the first ticket in the visible window, even if the
	// budget is too small — the column's MaxHeight clip is the safety net.
	if endIdx == ticketOffset && ticketOffset < len(tickets) {
		endIdx = ticketOffset + 1
	}

	hasMoreAbove := ticketOffset > 0
	hasMoreBelow := endIdx < len(tickets)

	indicatorStyle := lipgloss.NewStyle().
		Foreground(m.colors.muted).
		Width(width - 4).
		Align(lipgloss.Center)

	var ticketViews []string

	if hasMoreAbove {
		ticketViews = append(ticketViews, indicatorStyle.Render(fmt.Sprintf("▲ %d more", ticketOffset)))
	}

	for i := ticketOffset; i < endIdx; i++ {
		ticketViews = append(ticketViews, rendered[i])
	}

	if hasMoreBelow {
		remaining := len(tickets) - endIdx
		ticketViews = append(ticketViews, indicatorStyle.Render(fmt.Sprintf("▼ %d more", remaining)))
	}

	ticketsView := strings.Join(ticketViews, "\n")
	if len(tickets) == 0 {
		emptyIcon := "○"
		emptyText := "Drag or Space to move here"
		if col.Status == board.StatusBacklog {
			emptyIcon = "+"
			emptyText = "Press n to add a ticket"
		} else if col.Status == board.StatusDone {
			emptyIcon = "✓"
			emptyText = "Finished tickets land here"
		}
		emptyStyle := lipgloss.NewStyle().
			Foreground(m.colors.muted).
			Italic(true).
			Padding(2, 0).
			Width(width - 4).
			Align(lipgloss.Center)
		ticketsView = emptyStyle.Render(emptyIcon + "\n" + emptyText)
	}

	content := lipgloss.JoinVertical(lipgloss.Left, headerLine, "", ticketsView)

	border := columnBorder
	borderColor := m.colors.surface
	if isDragTarget {
		border = dragTargetBorder
		borderColor = m.colors.success
	} else if isActive {
		border = columnBorderActive
		borderColor = headerColor
	} else if isHovered {
		borderColor = m.colors.overlay
	}

	// MaxHeight prevents the column from ever exceeding the board area: if
	// the measured-pack loop above lets one too many cards through (e.g. a
	// last-minute width-dependent re-wrap), lipgloss clips from the bottom
	// instead of the terminal scrolling and clipping the top.
	maxHeight := m.boardAreaHeight()
	if maxHeight < 1 {
		maxHeight = 1
	}
	style := lipgloss.NewStyle().
		Border(border).
		BorderForeground(borderColor).
		Width(width).
		MaxHeight(maxHeight).
		Padding(0, 1)

	if !isLast {
		style = style.MarginRight(1)
	}

	return style.Render(content)
}

func (m *Model) renderTicket(ticket *board.Ticket, isSelected, isHovered bool, width int, columnColor lipgloss.Color, index, total int) string {
	pane, hasPane := m.panes[ticket.ID]
	isRunning := hasPane && pane.Running()

	effectiveStatus := ticket.AgentStatus

	var projectBadge string
	if proj := m.globalStore.GetProjectForTicket(ticket); proj != nil {
		shortName := proj.Name
		if len(shortName) > 12 {
			shortName = shortName[:10] + ".."
		}
		bracketStyle := lipgloss.NewStyle().Foreground(m.colors.info)
		textStyle := lipgloss.NewStyle().Foreground(m.colors.info).Bold(true)
		projectBadge = bracketStyle.Render("❨") + textStyle.Render(shortName) + bracketStyle.Render("❩")
	}

	var sessionBadge string
	switch effectiveStatus {
	case board.AgentWaiting:
		sessionBadge = lipgloss.NewStyle().
			Foreground(m.colors.secondary).
			Render("◐")
	case board.AgentIdle:
		if hasPane {
			sessionBadge = lipgloss.NewStyle().
				Foreground(m.colors.primary).
				Render("◆")
		}
	case board.AgentCompleted:
		sessionBadge = lipgloss.NewStyle().
			Foreground(m.colors.success).
			Render("✓")
	case board.AgentError:
		sessionBadge = lipgloss.NewStyle().
			Foreground(m.colors.err).
			Render("✗")
	}

	var attachedBadge string
	if m.daemonAttached[ticket.ID] > 0 {
		attachedBadge = lipgloss.NewStyle().
			Foreground(m.colors.info).
			Render("◉")
	}

	priorityBadge := m.renderPriorityBadge(ticket.Priority)

	var depBadge string
	blockedByCount := len(m.globalStore.GetBlockedBy(ticket.ID))
	blocksCount := len(m.globalStore.GetBlocks(ticket.ID))
	if blockedByCount > 0 || blocksCount > 0 {
		depStyle := lipgloss.NewStyle().Foreground(m.colors.muted)
		if blockedByCount > 0 && blocksCount > 0 {
			depBadge = depStyle.Render(fmt.Sprintf("⛓%d↑%d↓", blockedByCount, blocksCount))
		} else if blockedByCount > 0 {
			depBadge = depStyle.Render(fmt.Sprintf("⛓%d↑", blockedByCount))
		} else {
			depBadge = depStyle.Render(fmt.Sprintf("⛓%d↓", blocksCount))
		}
	}

	positionBadge := lipgloss.NewStyle().
		Foreground(m.colors.muted).
		Render(fmt.Sprintf("%d/%d", index, total))

	headerParts := []string{positionBadge}
	if priorityBadge != "" {
		headerParts = append(headerParts, priorityBadge)
	}
	if projectBadge != "" {
		headerParts = append(headerParts, projectBadge)
	}
	if depBadge != "" {
		headerParts = append(headerParts, depBadge)
	}
	if sessionBadge != "" {
		headerParts = append(headerParts, sessionBadge)
	}
	if attachedBadge != "" {
		headerParts = append(headerParts, attachedBadge)
	}
	headerLine := strings.Join(headerParts, "  ")

	// Every non-title card line is clamped to a single row. Title is allowed
	// to wrap to 2 rows above; the column packing loop measures each card's
	// actual height to budget visible cards.
	//
	// Width is (width - 2) to match the card's effective content width after
	// cardStyle.Padding(0,1); using `width` here causes lipgloss to wrap a
	// long line internally before MaxHeight(1) clips it — the first row
	// chosen is the pre-padded one, which then gets padded and is wider than
	// the card by 2 columns when re-rendered inside cardStyle.
	clampLine := func(s string) string {
		return lipgloss.NewStyle().Width(width - 2).MaxHeight(1).Render(s)
	}

	// Title is allowed to wrap to 2 rows; the column packing loop measures
	// each rendered ticket's actual height and pages accordingly, so longer
	// titles count as taller cards instead of being truncated.
	//
	// Width is (width - 2) to match the card's effective content width after
	// cardStyle.Padding(0,1). Without the -2, lipgloss wraps once at `width`
	// then the outer Padding(0,1) wraps the already-wrapped output a second
	// time, producing 3-4 rows from one long title (measured: a 100-rune
	// title at width=36 rendered at 11 rows instead of 9 with Width(width)).
	titleStyle := lipgloss.NewStyle().
		Foreground(m.colors.text).
		Bold(isSelected).
		Width(width - 2).
		MaxHeight(2)
	wrappedTitle := titleStyle.Render(ticket.Title)

	var descLine string
	if ticket.Description != "" {
		desc := ticket.Description
		if len(desc) > 60 {
			desc = desc[:57] + "..."
		}
		desc = strings.ReplaceAll(desc, "\n", " ")
		descLine = lipgloss.NewStyle().
			Foreground(m.colors.muted).
			Italic(true).
			Width(width - 2).
			MaxHeight(1).
			Render(desc)
	}

	var statusParts []string
	if ticket.AgentType != "" {
		agentBadge := lipgloss.NewStyle().
			Foreground(m.colors.base).
			Background(m.colors.primary).
			Padding(0, 1).
			Render(ticket.AgentType)
		statusParts = append(statusParts, agentBadge)
	}

	if effectiveStatus != board.AgentNone {
		var statusIcon, statusText string
		var statusColor lipgloss.Color
		switch effectiveStatus {
		case board.AgentIdle:
			statusIcon = "◆"
			statusText = "idle"
			statusColor = m.colors.primary
		case board.AgentWorking:
			statusIcon = m.spinner.View()
			statusText = "working"
			statusColor = m.colors.warning
		case board.AgentWaiting:
			statusIcon = "◐"
			statusText = "waiting"
			statusColor = m.colors.secondary
		case board.AgentCompleted:
			statusIcon = "✓"
			statusText = "done"
			statusColor = m.colors.success
		case board.AgentError:
			statusIcon = "✗"
			statusText = "error"
			statusColor = m.colors.err
		}
		statusStyle := lipgloss.NewStyle().Foreground(statusColor)
		statusParts = append(statusParts, statusStyle.Render(statusIcon+" "+statusText))
	}

	statusLine := strings.Join(statusParts, " ")

	var labelParts []string
	for _, label := range ticket.Labels {
		lbl := lipgloss.NewStyle().
			Foreground(m.colors.subtext).
			Background(m.colors.overlay).
			Padding(0, 1).
			Render(label)
		labelParts = append(labelParts, lbl)
	}
	labelsLine := strings.Join(labelParts, " ")

	lines := []string{clampLine(headerLine), wrappedTitle}
	if descLine != "" {
		lines = append(lines, descLine)
	}
	if statusLine != "" {
		lines = append(lines, clampLine(statusLine))
	}
	if labelsLine != "" {
		lines = append(lines, clampLine(labelsLine))
	}

	content := strings.Join(lines, "\n")

	var accentColor lipgloss.Color = m.colors.surface
	switch effectiveStatus {
	case board.AgentWorking:
		accentColor = m.colors.warning
	case board.AgentWaiting:
		accentColor = m.colors.secondary
	case board.AgentIdle:
		if hasPane {
			accentColor = m.colors.primary
		}
	case board.AgentCompleted:
		accentColor = m.colors.success
	case board.AgentError:
		accentColor = m.colors.err
	}
	if isRunning {
		accentColor = m.colors.success
	}

	border := ticketBorder
	borderColor := m.colors.surface

	if isHovered && !isSelected {
		borderColor = m.colors.overlay
	}

	if isSelected {
		border = ticketBorderSelected
		borderColor = columnColor
	}

	if isRunning {
		borderColor = m.colors.success
	}

	cardStyle := lipgloss.NewStyle().
		Border(border).
		BorderForeground(borderColor).
		BorderLeftForeground(accentColor).
		Padding(0, 1).
		MarginBottom(1).
		Width(width)

	return cardStyle.Render(content)
}

func (m *Model) renderStatusBar() string {
	type modeConfig struct {
		icon string
		bg   lipgloss.Color
	}
	modeConfigs := map[Mode]modeConfig{
		ModeNormal:        {"◆", m.colors.primary},
		ModeInsert:        {"✎", m.colors.success},
		ModeCommand:       {":", m.colors.secondary},
		ModeCreateTicket:  {"+", m.colors.success},
		ModeEditTicket:    {"✎", m.colors.warning},
		ModeAgentView:     {"▶", m.colors.info},
		ModeSettings:      {"⚙", m.colors.secondary},
		ModeHelp:          {"?", m.colors.primary},
		ModeConfirm:       {"!", m.colors.err},
		ModeConfirmExit:   {"⚠", m.colors.err},
		ModeFilter:        {"/", m.colors.info},
		ModeCreateProject: {"📁", m.colors.success},
	}
	cfg := modeConfigs[m.mode]
	if cfg.bg == "" {
		cfg = modeConfig{"◆", m.colors.primary}
	}
	modeStr := lipgloss.NewStyle().
		Foreground(m.colors.base).
		Background(cfg.bg).
		Bold(true).
		Padding(0, 1).
		Render(cfg.icon + " " + string(m.mode))

	sep := lipgloss.NewStyle().Foreground(m.colors.overlay).Render(" │ ")
	hintStyle := lipgloss.NewStyle().Foreground(m.colors.subtext)

	hints := m.contextualHints(hintStyle, sep)

	notif := ""
	if m.notification != "" {
		isError := strings.HasPrefix(m.notification, "Failed") ||
			strings.HasPrefix(m.notification, "Error") ||
			strings.Contains(m.notification, "failed")
		bgColor := m.colors.success
		icon := "✓"
		if isError {
			bgColor = m.colors.err
			icon = "✗"
		}
		notifBadge := lipgloss.NewStyle().
			Foreground(m.colors.base).
			Background(bgColor).
			Padding(0, 1).
			Render(icon + " " + m.notification)
		notif = notifBadge
	}

	left := lipgloss.JoinHorizontal(lipgloss.Center, modeStr, sep, hints)
	spacing := m.width - lipgloss.Width(left) - lipgloss.Width(notif)
	spacing = max(spacing, 0)

	return lipgloss.JoinHorizontal(lipgloss.Center, left, strings.Repeat(" ", spacing), notif)
}

func (m *Model) contextualHints(hintStyle lipgloss.Style, sep string) string {
	switch m.mode {
	case ModeFilter:
		return hintStyle.Render("Enter") + m.dimStyle().Render(" apply") + sep +
			hintStyle.Render("Esc") + m.dimStyle().Render(" cancel") + sep +
			m.dimStyle().Render("@project to filter by project")

	case ModeSettings:
		return hintStyle.Render("j/k") + m.dimStyle().Render(" navigate") + sep +
			hintStyle.Render("Enter") + m.dimStyle().Render(" select") + sep +
			hintStyle.Render("Esc") + m.dimStyle().Render(" close")

	case ModeCreateTicket, ModeEditTicket:
		action := "create"
		if m.mode == ModeEditTicket {
			action = "save"
		}
		return hintStyle.Render("Tab") + m.dimStyle().Render(" next") + sep +
			hintStyle.Render("Ctrl+S") + m.dimStyle().Render(" "+action) + sep +
			hintStyle.Render("Esc") + m.dimStyle().Render(" cancel")

	case ModeAgentView:
		return hintStyle.Render("Ctrl+G") + m.dimStyle().Render(" back to board") + sep +
			m.dimStyle().Render("Shift+click to select text")

	case ModeNormal:
		if m.sidebarFocused {
			return hintStyle.Render("j/k") + m.dimStyle().Render(" navigate") + sep +
				hintStyle.Render("Space/Enter") + m.dimStyle().Render(" toggle") + sep +
				hintStyle.Render("l") + m.dimStyle().Render(" board")
		}

		if m.filterQuery != "" || len(m.filterProjectIDs) > 0 {
			return hintStyle.Render("Esc") + m.dimStyle().Render(" clear filter") + sep +
				hintStyle.Render("/") + m.dimStyle().Render(" edit filter") + sep +
				hintStyle.Render("?") + m.dimStyle().Render(" help")
		}

		ticket := m.selectedTicket()
		if ticket != nil {
			if _, hasPane := m.panes[ticket.ID]; hasPane {
				return hintStyle.Render("Enter/s") + m.dimStyle().Render(" open agent") + sep +
					hintStyle.Render("S") + m.dimStyle().Render(" stop agent") + sep +
					hintStyle.Render("Space") + m.dimStyle().Render(" move") + sep +
					hintStyle.Render("?") + m.dimStyle().Render(" help")
			}
			if ticket.Status == board.StatusInProgress {
				return hintStyle.Render("Enter/s") + m.dimStyle().Render(" spawn agent") + sep +
					hintStyle.Render("Space") + m.dimStyle().Render(" move") + sep +
					hintStyle.Render("e") + m.dimStyle().Render(" edit") + sep +
					hintStyle.Render("?") + m.dimStyle().Render(" help")
			}
		}

		return hintStyle.Render("h/l") + m.dimStyle().Render(" columns") + sep +
			hintStyle.Render("n") + m.dimStyle().Render(" new") + sep +
			hintStyle.Render("Space") + m.dimStyle().Render(" move") + sep +
			hintStyle.Render("/") + m.dimStyle().Render(" search") + sep +
			hintStyle.Render("?") + m.dimStyle().Render(" help")

	default:
		return hintStyle.Render("Esc") + m.dimStyle().Render(" back") + sep +
			hintStyle.Render("?") + m.dimStyle().Render(" help")
	}
}

func (m *Model) renderHelp() string {
	titleStyle := lipgloss.NewStyle().
		Foreground(m.colors.primary).
		Bold(true)

	sectionStyle := lipgloss.NewStyle().
		Foreground(m.colors.secondary).
		Bold(true)

	keyStyle := lipgloss.NewStyle().
		Foreground(m.colors.info).
		Bold(true)

	descStyle := lipgloss.NewStyle().
		Foreground(m.colors.subtext)

	sepStyle := lipgloss.NewStyle().
		Foreground(m.colors.surface)

	sep := sepStyle.Render("────────────────────────────────────────────")

	help := titleStyle.Render("◈ Keyboard Shortcuts") + "\n\n" +
		sep + "\n" +
		sectionStyle.Render("  🧭 Navigation") + "                 " + sectionStyle.Render("📝 Actions") + "\n" +
		sep + "\n" +
		"  " + keyStyle.Render("h/l") + descStyle.Render("   Move between columns  ") + keyStyle.Render("n") + descStyle.Render("       New ticket") + "\n" +
		"  " + keyStyle.Render("j/k") + descStyle.Render("   Move between tickets  ") + keyStyle.Render("e") + descStyle.Render("       Edit ticket") + "\n" +
		"  " + keyStyle.Render("g") + descStyle.Render("     Go to first ticket    ") + keyStyle.Render("d") + descStyle.Render("       Delete ticket") + "\n" +
		"  " + keyStyle.Render("G") + descStyle.Render("     Go to last ticket     ") + keyStyle.Render("Space") + descStyle.Render("   Move forward") + "\n" +
		"  " + keyStyle.Render(" ") + descStyle.Render("                            ") + keyStyle.Render("-") + descStyle.Render("       Move backward") + "\n" +
		"  " + keyStyle.Render(" ") + descStyle.Render("                            ") + keyStyle.Render("K/J") + descStyle.Render("     Raise/lower priority") + "\n\n" +
		sep + "\n" +
		sectionStyle.Render("  📂 Sidebar") + "                    " + sectionStyle.Render("🤖 Agent") + "\n" +
		sep + "\n" +
		"  " + keyStyle.Render("[") + descStyle.Render("     Toggle sidebar        ") + keyStyle.Render("s/Enter") + descStyle.Render(" Open agent") + "\n" +
		"  " + keyStyle.Render("h") + descStyle.Render("     Enter sidebar         ") + keyStyle.Render("S") + descStyle.Render("       Stop agent") + "\n" +
		"  " + keyStyle.Render("l") + descStyle.Render("     Exit sidebar          ") + keyStyle.Render("Ctrl+g") + descStyle.Render("  Exit agent view") + "\n" +
		"  " + keyStyle.Render("j/k") + descStyle.Render("   Navigate projects") + "\n\n" +
		sep + "\n" +
		sectionStyle.Render("  👁 View") + "\n" +
		sep + "\n" +
		"  " + keyStyle.Render("/") + descStyle.Render("     Search/filter         ") + keyStyle.Render("O") + descStyle.Render("       Settings") + "\n" +
		"  " + keyStyle.Render("o") + descStyle.Render("     Cycle sort order      ") + keyStyle.Render("?") + descStyle.Render("       Toggle help") + "\n" +
		"  " + keyStyle.Render("w") + descStyle.Render("     Cycle session filter  ") + keyStyle.Render("q") + descStyle.Render("       Quit") + "\n\n" +
		sep + "\n" +
		"  " + lipgloss.NewStyle().Foreground(m.colors.warning).Render("💡") + m.dimStyle().Render(" Tip: Hold Shift to select text in agent view") + "\n\n" +
		"  " + m.dimStyle().Render("Press any key to close")

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.colors.primary).
		Padding(1, 2).
		Render(help)
}

func (m *Model) renderConfirmDialog() string {
	titleStyle := lipgloss.NewStyle().
		Foreground(m.colors.err).
		Bold(true)

	content := titleStyle.Render("⚠ Confirm") + "\n\n" +
		"  " + lipgloss.NewStyle().Foreground(m.colors.text).Render(m.confirmMsg) + "\n\n" +
		"  " + lipgloss.NewStyle().Foreground(m.colors.success).Render("[y]") + m.dimStyle().Render(" Yes    ") +
		lipgloss.NewStyle().Foreground(m.colors.err).Render("[n]") + m.dimStyle().Render(" No    ") +
		lipgloss.NewStyle().Foreground(m.colors.muted).Render("[Esc]") + m.dimStyle().Render(" Cancel")

	return lipgloss.NewStyle().
		Border(columnBorder).
		BorderForeground(m.colors.err).
		Padding(1, 2).
		Render(content)
}

func (m *Model) renderChoiceDialog() string {
	if !m.showChoice {
		return ""
	}

	titleStyle := lipgloss.NewStyle().
		Foreground(m.colors.err).
		Bold(true)
	textStyle := lipgloss.NewStyle().Foreground(m.colors.text)
	keyStyle := lipgloss.NewStyle().Foreground(m.colors.success)
	escKeyStyle := lipgloss.NewStyle().Foreground(m.colors.muted)

	var b strings.Builder
	b.WriteString(titleStyle.Render("⚠ Confirm"))
	b.WriteString("\n\n")
	b.WriteString("  ")
	b.WriteString(textStyle.Render(m.choiceMsg))
	b.WriteString("\n\n")

	for _, c := range m.choices {
		b.WriteString("  ")
		b.WriteString(keyStyle.Render(fmt.Sprintf("[%s]", string(c.Key))))
		b.WriteString(m.dimStyle().Render(" " + c.Label))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString("  ")
	b.WriteString(escKeyStyle.Render("[Esc]"))
	b.WriteString(m.dimStyle().Render(" Cancel"))

	return lipgloss.NewStyle().
		Border(columnBorder).
		BorderForeground(m.colors.err).
		Padding(1, 2).
		Render(b.String())
}

func (m *Model) renderShuttingDown() string {
	count := m.RunningAgentCount()
	msg := fmt.Sprintf("Stopping %d agent(s)...", count)

	titleStyle := lipgloss.NewStyle().
		Foreground(m.colors.warning).
		Bold(true)

	content := titleStyle.Render(m.spinner.View()+" Shutting Down") + "\n\n" +
		"  " + lipgloss.NewStyle().Foreground(m.colors.text).Render(msg)

	dialog := lipgloss.NewStyle().
		Border(columnBorder).
		BorderForeground(m.colors.warning).
		Padding(1, 2).
		Render(content)

	return lipgloss.Place(
		m.width,
		m.height,
		lipgloss.Center,
		lipgloss.Center,
		dialog,
	)
}

func (m *Model) renderSpawning() string {
	agentName := m.spawningAgent
	if agentName == "" {
		agentName = "agent"
	}

	titleStyle := lipgloss.NewStyle().
		Foreground(m.colors.success).
		Bold(true)

	content := titleStyle.Render(m.spinner.View()+" Starting "+agentName) + "\n\n" +
		"  " + m.dimStyle().Render("[Esc] Cancel")

	dialog := lipgloss.NewStyle().
		Border(columnBorder).
		BorderForeground(m.colors.success).
		Padding(1, 2).
		Render(content)

	return lipgloss.Place(
		m.width,
		m.height,
		lipgloss.Center,
		lipgloss.Center,
		dialog,
	)
}

const formOverhead = 10 // border(2) + padding(2) + title+blanks(3) + footer+blanks(3)

func (m *Model) formViewportHeight() int {
	available := m.height - formOverhead
	if available < 10 {
		available = 10
	}
	return available
}

func (m *Model) renderTicketForm() string {
	isEdit := m.mode == ModeEditTicket
	formTitle := "New Ticket"
	actionText := "Create"
	if isEdit {
		formTitle = "Edit Ticket"
		actionText = "Save"
	}

	titleStyle := lipgloss.NewStyle().
		Foreground(m.colors.success).
		Bold(true)

	labelStyle := lipgloss.NewStyle().Foreground(m.colors.subtext)
	activeLabelStyle := lipgloss.NewStyle().Foreground(m.colors.info).Bold(true)
	lockedStyle := lipgloss.NewStyle().Foreground(m.colors.muted).Italic(true)
	descriptionStyle := lipgloss.NewStyle().Foreground(m.colors.muted).Italic(true)

	titleLabel := labelStyle
	descLabel := labelStyle
	branchLabel := labelStyle
	labelsLabel := labelStyle
	priorityLabel := labelStyle
	worktreeLabel := labelStyle
	agentLabel := labelStyle
	blockerLabel := labelStyle
	projectLabel := labelStyle

	fieldStartLines := make(map[int]int)
	currentLine := 0

	switch m.ticketFormField {
	case formFieldTitle:
		titleLabel = activeLabelStyle
	case formFieldDescription:
		descLabel = activeLabelStyle
	case formFieldBranch:
		branchLabel = activeLabelStyle
	case formFieldLabels:
		labelsLabel = activeLabelStyle
	case formFieldPriority:
		priorityLabel = activeLabelStyle
	case formFieldWorktree:
		worktreeLabel = activeLabelStyle
	case formFieldAgent:
		agentLabel = activeLabelStyle
	case formFieldBlockedBy:
		blockerLabel = activeLabelStyle
	case formFieldProject:
		projectLabel = activeLabelStyle
	}

	var branchField string
	var branchDesc string
	if m.branchLocked {
		branchLabel = lockedStyle
		branchField = lockedStyle.Render(m.branchInput.Value() + " (locked)")
		branchDesc = descriptionStyle.Render("Branch is locked after worktree creation")
	} else {
		branchField = m.branchInput.View()
		branchDesc = descriptionStyle.Render("Auto-generated from title if left empty")
	}

	priorityField := m.renderPrioritySelector()
	worktreeField := m.renderWorktreeSelector()
	agentField := m.renderAgentSelector()
	blockerField := m.renderBlockerSelector()
	projectField := m.renderProjectSelector()

	titleCharCount := fmt.Sprintf("%d/100", len(m.titleInput.Value()))
	titleCharStyle := lipgloss.NewStyle().Foreground(m.colors.muted)
	if len(m.titleInput.Value()) > 80 {
		titleCharStyle = lipgloss.NewStyle().Foreground(m.colors.warning)
	}
	if len(m.titleInput.Value()) >= 100 {
		titleCharStyle = lipgloss.NewStyle().Foreground(m.colors.err)
	}

	focusIndicator := lipgloss.NewStyle().Foreground(m.colors.info).Render("▸ ")
	noFocus := "  "

	titleFocus, descFocus, branchFocus, labelsFocus, priorityFocus, worktreeFocus, agentFocus, blockerFocus, projectFocus := noFocus, noFocus, noFocus, noFocus, noFocus, noFocus, noFocus, noFocus, noFocus
	switch m.ticketFormField {
	case formFieldTitle:
		titleFocus = focusIndicator
	case formFieldDescription:
		descFocus = focusIndicator
	case formFieldBranch:
		branchFocus = focusIndicator
	case formFieldLabels:
		labelsFocus = focusIndicator
	case formFieldPriority:
		priorityFocus = focusIndicator
	case formFieldWorktree:
		worktreeFocus = focusIndicator
	case formFieldAgent:
		agentFocus = focusIndicator
	case formFieldBlockedBy:
		blockerFocus = focusIndicator
	case formFieldProject:
		projectFocus = focusIndicator
	}

	var lines []string
	fieldEndLines := make(map[int]int)

	fieldStartLines[formFieldTitle] = currentLine
	lines = append(lines, titleFocus+titleLabel.Render("Title")+"  "+titleCharStyle.Render(titleCharCount))
	lines = append(lines, "  "+descriptionStyle.Render("Brief summary of the task"))
	lines = append(lines, "  "+m.titleInput.View())
	lines = append(lines, "")
	fieldEndLines[formFieldTitle] = len(lines) - 1
	currentLine = len(lines)

	fieldStartLines[formFieldDescription] = currentLine
	lines = append(lines, descFocus+descLabel.Render("Description"))
	lines = append(lines, "  "+descriptionStyle.Render("Details, context, or acceptance criteria"))
	descLines := strings.Split(m.descInput.View(), "\n")
	for _, dl := range descLines {
		lines = append(lines, "  "+dl)
	}
	lines = append(lines, "")
	fieldEndLines[formFieldDescription] = len(lines) - 1
	currentLine = len(lines)

	fieldStartLines[formFieldBranch] = currentLine
	lines = append(lines, branchFocus+branchLabel.Render("Branch"))
	lines = append(lines, "  "+branchDesc)
	lines = append(lines, "  "+branchField)
	lines = append(lines, "")
	fieldEndLines[formFieldBranch] = len(lines) - 1
	currentLine = len(lines)

	fieldStartLines[formFieldLabels] = currentLine
	lines = append(lines, labelsFocus+labelsLabel.Render("Labels"))
	lines = append(lines, "  "+descriptionStyle.Render("Comma-separated tags (e.g. bug, urgent)"))
	lines = append(lines, "  "+m.labelsInput.View())
	lines = append(lines, "")
	fieldEndLines[formFieldLabels] = len(lines) - 1
	currentLine = len(lines)

	fieldStartLines[formFieldPriority] = currentLine
	lines = append(lines, priorityFocus+priorityLabel.Render("Priority"))
	lines = append(lines, "  "+descriptionStyle.Render("1 = highest, 5 = lowest"))
	lines = append(lines, "  "+priorityField)
	lines = append(lines, "")
	fieldEndLines[formFieldPriority] = len(lines) - 1
	currentLine = len(lines)

	fieldStartLines[formFieldWorktree] = currentLine
	lines = append(lines, worktreeFocus+worktreeLabel.Render("Worktree"))
	lines = append(lines, "  "+descriptionStyle.Render("Use isolated worktree or work in main repo"))
	lines = append(lines, "  "+worktreeField)
	lines = append(lines, "")
	fieldEndLines[formFieldWorktree] = len(lines) - 1
	currentLine = len(lines)

	fieldStartLines[formFieldAgent] = currentLine
	lines = append(lines, agentFocus+agentLabel.Render("Agent"))
	lines = append(lines, "  "+descriptionStyle.Render("AI agent to use for this ticket"))
	lines = append(lines, "  "+agentField)
	lines = append(lines, "")
	fieldEndLines[formFieldAgent] = len(lines) - 1
	currentLine = len(lines)

	fieldStartLines[formFieldBlockedBy] = currentLine
	lines = append(lines, blockerFocus+blockerLabel.Render("Blocked By"))
	lines = append(lines, "  "+descriptionStyle.Render("Tickets that must complete before this one"))
	blockerLines := strings.Split(blockerField, "\n")
	for _, bl := range blockerLines {
		lines = append(lines, "  "+bl)
	}
	fieldEndLines[formFieldBlockedBy] = len(lines) - 1
	currentLine = len(lines)

	lines = append(lines, "")
	currentLine = len(lines)
	fieldStartLines[formFieldProject] = currentLine
	lines = append(lines, projectFocus+projectLabel.Render("Project"))
	lines = append(lines, "  "+descriptionStyle.Render("Repository where this ticket belongs"))
	projectLines := strings.Split(projectField, "\n")
	for _, pl := range projectLines {
		lines = append(lines, "  "+pl)
	}
	fieldEndLines[formFieldProject] = len(lines) - 1

	m.formFieldLines = fieldStartLines

	viewportHeight := m.formViewportHeight()
	totalLines := len(lines)
	needsScroll := totalLines > viewportHeight

	if needsScroll {
		startLine, hasStart := fieldStartLines[m.ticketFormField]
		endLine, hasEnd := fieldEndLines[m.ticketFormField]
		if hasStart && hasEnd {
			fieldHeight := endLine - startLine + 1
			effectiveViewport := viewportHeight - 2

			if fieldHeight <= effectiveViewport {
				if endLine >= m.formScrollOffset+effectiveViewport {
					m.formScrollOffset = endLine - effectiveViewport + 1
				}
				if startLine < m.formScrollOffset {
					m.formScrollOffset = startLine
				}
			} else {
				m.formScrollOffset = startLine
			}
		}
		maxOffset := totalLines - viewportHeight
		if maxOffset < 0 {
			maxOffset = 0
		}
		if m.formScrollOffset > maxOffset {
			m.formScrollOffset = maxOffset
		}
		if m.formScrollOffset < 0 {
			m.formScrollOffset = 0
		}
	} else {
		m.formScrollOffset = 0
	}

	var visibleLines []string
	scrollIndicatorStyle := lipgloss.NewStyle().Foreground(m.colors.info).Bold(true)

	hasAboveIndicator := needsScroll && m.formScrollOffset > 0
	hasBelowIndicator := needsScroll && m.formScrollOffset+viewportHeight < totalLines

	availableForContent := viewportHeight
	if hasAboveIndicator {
		availableForContent--
	}
	if hasBelowIndicator {
		availableForContent--
	}

	endLine := m.formScrollOffset + availableForContent
	if endLine > totalLines {
		endLine = totalLines
	}

	if hasAboveIndicator {
		visibleLines = append(visibleLines, scrollIndicatorStyle.Render(fmt.Sprintf("  ▲ %d more above", m.formScrollOffset)))
	}

	for i := m.formScrollOffset; i < endLine; i++ {
		visibleLines = append(visibleLines, lines[i])
	}

	if hasBelowIndicator {
		belowCount := totalLines - endLine
		visibleLines = append(visibleLines, scrollIndicatorStyle.Render(fmt.Sprintf("  ▼ %d more below", belowCount)))
	}

	content := titleStyle.Render("◈ "+formTitle) + "\n\n" + strings.Join(visibleLines, "\n")

	footerHints := lipgloss.NewStyle().Foreground(m.colors.info).Render("[Tab]") + m.dimStyle().Render(" Next  ") +
		lipgloss.NewStyle().Foreground(m.colors.success).Render("[Ctrl+S]") + m.dimStyle().Render(" "+actionText+"  ") +
		lipgloss.NewStyle().Foreground(m.colors.muted).Render("[Esc]") + m.dimStyle().Render(" Cancel")
	content += "\n\n  " + footerHints

	formWidth := min(60, m.width-4)
	if formWidth < 40 {
		formWidth = 40
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.colors.success).
		Padding(1, 2).
		Width(formWidth).
		Render(content)
}

func (m *Model) renderPrioritySelector() string {
	priorities := []struct {
		level int
		label string
		color lipgloss.Color
	}{
		{1, "Critical", m.colors.err},
		{2, "High", lipgloss.Color("#fab387")},
		{3, "Medium", m.colors.warning},
		{4, "Low", m.colors.primary},
		{5, "Lowest", m.colors.muted},
	}

	var parts []string
	for _, p := range priorities {
		style := lipgloss.NewStyle().Foreground(p.color)
		if m.ticketPriority == p.level {
			style = style.Bold(true).Background(m.colors.surface).Padding(0, 1)
			parts = append(parts, style.Render(fmt.Sprintf("● %s", p.label)))
		} else {
			parts = append(parts, style.Render(fmt.Sprintf("○ %d", p.level)))
		}
	}

	hint := ""
	if m.ticketFormField == formFieldPriority {
		hint = "  " + m.dimStyle().Render("← → or 1-5")
	}

	return strings.Join(parts, "  ") + hint
}

func (m *Model) renderWorktreeSelector() string {
	worktreeStyle := lipgloss.NewStyle().Foreground(m.colors.success)
	mainRepoStyle := lipgloss.NewStyle().Foreground(m.colors.warning)

	var worktreeOption, mainOption string
	if m.ticketUseWorktree {
		worktreeStyle = worktreeStyle.Bold(true).Background(m.colors.surface).Padding(0, 1)
		worktreeOption = worktreeStyle.Render("● Worktree")
		mainOption = mainRepoStyle.Render("○ Main Repo")
	} else {
		mainRepoStyle = mainRepoStyle.Bold(true).Background(m.colors.surface).Padding(0, 1)
		worktreeOption = worktreeStyle.Render("○ Worktree")
		mainOption = mainRepoStyle.Render("● Main Repo")
	}

	hint := ""
	if m.ticketFormField == formFieldWorktree {
		hint = "  " + m.dimStyle().Render("Space to toggle")
	}

	return worktreeOption + "  " + mainOption + hint
}

func (m *Model) renderAgentSelector() string {
	agents := m.getAgentNames()
	if len(agents) == 0 {
		return m.dimStyle().Render("No agents configured")
	}

	var parts []string
	for _, agent := range agents {
		style := lipgloss.NewStyle().Foreground(m.colors.primary)
		if m.ticketAgent == agent {
			style = style.Bold(true).Background(m.colors.surface).Padding(0, 1)
			parts = append(parts, style.Render("● "+agent))
		} else {
			parts = append(parts, style.Render("○ "+agent))
		}
	}

	hint := ""
	if m.ticketFormField == formFieldAgent && !m.agentLocked {
		hint = "  " + m.dimStyle().Render("← → to select")
	} else if m.agentLocked {
		hint = "  " + m.dimStyle().Render("(locked - agent already spawned)")
	}

	return strings.Join(parts, "  ") + hint
}

func (m *Model) renderBlockerSelector() string {
	if len(m.blockerCandidates) == 0 {
		return m.dimStyle().Render("No other tickets available")
	}

	if m.ticketFormField != formFieldBlockedBy {
		count := len(m.selectedBlockers)
		if count == 0 {
			return m.dimStyle().Render("None selected")
		}
		var names []string
		for id := range m.selectedBlockers {
			if t, _ := m.globalStore.Get(id); t != nil {
				name := t.Title
				if len(name) > 20 {
					name = name[:18] + ".."
				}
				names = append(names, name)
			}
		}
		sort.Strings(names)
		return lipgloss.NewStyle().Foreground(m.colors.info).Render(strings.Join(names, ", "))
	}

	var lines []string
	lines = append(lines, m.blockerFilterInput.View())
	lines = append(lines, "")

	visibleCandidates := m.getFilteredBlockerCandidates()
	maxVisible := 5

	for i, ticket := range visibleCandidates {
		if i >= maxVisible {
			remaining := len(visibleCandidates) - maxVisible
			lines = append(lines, m.dimStyle().Render(fmt.Sprintf("  ... and %d more", remaining)))
			break
		}

		name := ticket.Title
		if len(name) > 30 {
			name = name[:28] + ".."
		}

		proj := m.globalStore.GetProjectForTicket(ticket)
		projName := ""
		if proj != nil {
			projName = proj.Name
			if len(projName) > 10 {
				projName = projName[:8] + ".."
			}
		}

		isSelected := m.selectedBlockers[ticket.ID]
		isHovered := i == m.blockerListIndex

		checkbox := "[ ] "
		checkboxStyle := lipgloss.NewStyle().Foreground(m.colors.muted)
		if isSelected {
			checkbox = "[✓] "
			checkboxStyle = lipgloss.NewStyle().Foreground(m.colors.success).Bold(true)
		}

		cursor := "  "
		nameStyle := lipgloss.NewStyle().Foreground(m.colors.text)
		projStyle := lipgloss.NewStyle().Foreground(m.colors.muted)

		if isHovered {
			cursor = lipgloss.NewStyle().Foreground(m.colors.info).Render("▸ ")
			nameStyle = nameStyle.Bold(true).Foreground(m.colors.info)
			projStyle = projStyle.Foreground(m.colors.subtext)
		}

		line := cursor + checkboxStyle.Render(checkbox) + nameStyle.Render(name)
		if projName != "" {
			line += "  " + projStyle.Render("❨"+projName+"❩")
		}
		lines = append(lines, line)
	}

	if len(visibleCandidates) == 0 {
		lines = append(lines, m.dimStyle().Render("No matching tickets"))
	}

	lines = append(lines, "")
	lines = append(lines, m.dimStyle().Render("↑↓ navigate  Space/Enter toggle  Tab next"))

	return strings.Join(lines, "\n")
}

func (m *Model) renderWithOverlay(overlay string) string {
	return lipgloss.Place(
		m.width,
		m.height,
		lipgloss.Center,
		lipgloss.Center,
		overlay,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(m.colors.base),
	)
}

func (m *Model) renderSettingsView() string {
	titleStyle := lipgloss.NewStyle().
		Foreground(m.colors.secondary).
		Bold(true)

	labelStyle := lipgloss.NewStyle().
		Foreground(m.colors.subtext)

	valueStyle := lipgloss.NewStyle().
		Foreground(m.colors.text)

	descStyle := lipgloss.NewStyle().
		Foreground(m.colors.muted).
		Italic(true)

	selectedLabelStyle := lipgloss.NewStyle().
		Foreground(m.colors.secondary).
		Bold(true)

	var lines []string
	lines = append(lines, titleStyle.Render("◈ Settings"))
	lines = append(lines, "")

	for i, field := range settingsFields {
		label := field.label
		value := m.getSettingsValue(field.key)

		cursor := "  "
		lStyle := labelStyle
		vStyle := valueStyle

		if i == m.settingsIndex {
			cursor = lipgloss.NewStyle().Foreground(m.colors.secondary).Render("▸ ")
			lStyle = selectedLabelStyle
			vStyle = lipgloss.NewStyle().Foreground(m.colors.info)
		}

		line := cursor + lStyle.Render(fmt.Sprintf("%-18s", label)) + " " + vStyle.Render(value)
		lines = append(lines, line)
		lines = append(lines, "    "+descStyle.Render(field.description))

		if i == m.settingsIndex && m.settingsEditing && field.kind == "theme" {
			lines = append(lines, m.renderThemeDropdown())
		}

		lines = append(lines, "")
	}

	lines = append(lines, m.dimStyle().Render("  Config file: ~/.config/openkanban/config.json"))
	lines = append(lines, "")

	field := settingsFields[m.settingsIndex]
	var actionHint string
	switch field.kind {
	case "toggle":
		actionHint = "Toggle"
	case "project", "theme":
		actionHint = "Select"
	default:
		actionHint = "Edit"
	}

	lines = append(lines, "  "+lipgloss.NewStyle().Foreground(m.colors.info).Render("[Enter]")+m.dimStyle().Render(" "+actionHint+"  ")+
		lipgloss.NewStyle().Foreground(m.colors.muted).Render("[Esc]")+m.dimStyle().Render(" Close"))

	content := strings.Join(lines, "\n")

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.colors.secondary).
		Padding(1, 2).
		Render(content)
}

func (m *Model) renderAgentView() string {
	pane, ok := m.panes[m.focusedPane]
	if !ok {
		return "No pane focused"
	}

	var b strings.Builder

	ticket, _ := m.globalStore.Get(m.focusedPane)
	title := "Agent"
	agentType := ""
	projectName := ""
	priority := 0
	var agentStatus board.AgentStatus
	var sessionDuration string
	if ticket != nil {
		title = ticket.Title
		agentType = ticket.AgentType
		priority = ticket.Priority
		agentStatus = ticket.AgentStatus
		if proj := m.globalStore.GetProjectForTicket(ticket); proj != nil {
			projectName = proj.Name
		}
		if ticket.AgentSpawnedAt != nil {
			duration := time.Since(*ticket.AgentSpawnedAt)
			sessionDuration = formatDuration(duration)
		}
	}

	titleStyle := lipgloss.NewStyle().
		Foreground(m.colors.primary).
		Bold(true)

	var headerParts []string
	if pri := m.renderPriorityBadge(priority); pri != "" {
		headerParts = append(headerParts, pri)
	}
	headerParts = append(headerParts, titleStyle.Render(title))

	if projectName != "" {
		projBadge := lipgloss.NewStyle().
			Foreground(m.colors.base).
			Background(m.colors.info).
			Padding(0, 1).
			Render(projectName)
		headerParts = append(headerParts, projBadge)
	}

	if agentType != "" {
		agentBadge := lipgloss.NewStyle().
			Foreground(m.colors.base).
			Background(m.colors.primary).
			Padding(0, 1).
			Render(agentType)
		headerParts = append(headerParts, agentBadge)
	}

	if pill := m.renderAgentStatusPill(agentStatus); pill != "" {
		headerParts = append(headerParts, pill)
	}

	// Session duration ticks since AgentSpawnedAt. Suppress it when the
	// agent has reported completion — the "✓ done" pill carries the
	// state, and a still-ticking counter would read as "still running".
	if sessionDuration != "" && agentStatus != board.AgentCompleted {
		durationBadge := lipgloss.NewStyle().
			Foreground(m.colors.muted).
			Render("⏱ " + sessionDuration)
		headerParts = append(headerParts, durationBadge)
	}

	header := strings.Join(headerParts, "  ")

	var depsLine string
	if ticket != nil {
		blockedBy := m.globalStore.GetBlockedBy(ticket.ID)
		blocks := m.globalStore.GetBlocks(ticket.ID)
		if len(blockedBy) > 0 || len(blocks) > 0 {
			depStyle := lipgloss.NewStyle().Foreground(m.colors.muted)
			var depParts []string
			if len(blockedBy) > 0 {
				var names []string
				for _, t := range blockedBy {
					names = append(names, t.Title)
				}
				depParts = append(depParts, "⛓↑ "+strings.Join(names, ", "))
			}
			if len(blocks) > 0 {
				var names []string
				for _, t := range blocks {
					names = append(names, t.Title)
				}
				depParts = append(depParts, "⛓↓ "+strings.Join(names, ", "))
			}
			depsLine = depStyle.Render(strings.Join(depParts, "  "))
		}
	}

	activePaneCount := 0
	paneIndex := 0
	for id, p := range m.panes {
		if p.Running() {
			activePaneCount++
			if id == m.focusedPane {
				paneIndex = activePaneCount
			}
		}
	}

	paneIndicator := lipgloss.NewStyle().
		Foreground(m.colors.muted).
		Render(fmt.Sprintf("[%d/%d]", paneIndex, activePaneCount))

	// Scroll indicator when viewport is scrolled back
	scrollIndicator := ""
	if offset := pane.ViewportOffset(); offset > 0 {
		scrollbackLen := pane.ScrollbackLen()
		scrollStyle := lipgloss.NewStyle().
			Foreground(m.colors.warning).
			Bold(true)
		scrollIndicator = scrollStyle.Render(fmt.Sprintf("↑%d/%d", offset, scrollbackLen)) + "  "
	}

	keyStyle := lipgloss.NewStyle().Foreground(m.colors.info)
	hints := scrollIndicator + paneIndicator + "  " +
		keyStyle.Render("Ctrl+g") + m.dimStyle().Render(" Board")

	spacing := m.width - lipgloss.Width(header) - lipgloss.Width(hints)
	// At least one cell of separation keeps the bar legible when content
	// is wide enough to butt the right-hand hints against the header.
	if spacing < 1 {
		spacing = 1
	}

	// Overlay-tinted background spans the full width to mark the chrome
	// as a distinct band over the embedded PTY. Using `overlay` rather
	// than `surface` because surface alone is too subtle — at a glance
	// it's not obvious the session is encapsulated. Inner badges keep
	// their own backgrounds; spacing cells inherit the bar's tint.
	barStyle := lipgloss.NewStyle().Background(m.colors.overlay).Width(m.width)
	bar := barStyle.Render(header + strings.Repeat(" ", spacing) + hints)
	b.WriteString(bar)
	b.WriteString("\n")

	if depsLine != "" {
		b.WriteString(barStyle.Render(depsLine))
		b.WriteString("\n")
	}

	// Heavy primary-colored rule across the full width nails the boundary
	// between openkanban chrome and the embedded PTY. Without it the
	// surface-tinted band reads as "maybe a status line" — with it, the
	// "this session is wrapped" affordance is unambiguous.
	separatorStyle := lipgloss.NewStyle().Foreground(m.colors.primary)
	b.WriteString(separatorStyle.Render(strings.Repeat("━", m.width)))
	b.WriteString("\n")

	b.WriteString(pane.View())

	return b.String()
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	if mins == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh%dm", hours, mins)
}

// agentStatusGlyph maps an AgentStatus to the (icon, label) pair used
// in the embedded-session title bar. Returns empty strings for
// AgentNone (no badge). The icons match renderTicket's sessionBadge
// where they overlap; AgentWorking adds a solid dot since the card
// view doesn't render a badge for working agents.
func agentStatusGlyph(s board.AgentStatus) (icon, label string) {
	switch s {
	case board.AgentWorking:
		return "●", "working"
	case board.AgentWaiting:
		return "◐", "waiting"
	case board.AgentIdle:
		return "◆", "idle"
	case board.AgentCompleted:
		return "✓", "done"
	case board.AgentError:
		return "✗", "error"
	}
	return "", ""
}

// renderAgentStatusPill returns a styled "<icon> <label>" pill for the
// embedded-session title bar, or "" for AgentNone. Color tracks status
// severity (working=success, waiting=warning, completed=success-dim,
// error=err, idle=muted).
func (m *Model) renderAgentStatusPill(s board.AgentStatus) string {
	icon, label := agentStatusGlyph(s)
	if icon == "" {
		return ""
	}
	var color lipgloss.Color
	switch s {
	case board.AgentWorking:
		color = m.colors.success
	case board.AgentWaiting:
		color = m.colors.warning
	case board.AgentIdle:
		color = m.colors.muted
	case board.AgentCompleted:
		color = m.colors.success
	case board.AgentError:
		color = m.colors.err
	default:
		color = m.colors.muted
	}
	return lipgloss.NewStyle().Foreground(color).Render(icon + " " + label)
}

// priorityGlyph returns the two-cell glyph used for a ticket priority,
// or "" if the priority is out of the 1..5 range. The convention is
// shared between the card view and the agent-view title bar.
func priorityGlyph(p int) string {
	switch p {
	case 1:
		return "⌃⌃"
	case 2:
		return "⌃⎯"
	case 3:
		return "⎯⎯"
	case 4:
		return "⎯⌄"
	case 5:
		return "⌄⌄"
	}
	return ""
}

// renderPriorityBadge returns a styled priority glyph. Empty string
// for out-of-range priorities.
func (m *Model) renderPriorityBadge(p int) string {
	glyph := priorityGlyph(p)
	if glyph == "" {
		return ""
	}
	var color lipgloss.Color
	switch p {
	case 1:
		color = m.colors.err
	case 2:
		color = lipgloss.Color("#fab387")
	case 3:
		color = m.colors.warning
	case 4:
		color = m.colors.primary
	case 5:
		color = m.colors.muted
	}
	return lipgloss.NewStyle().Foreground(color).Bold(true).Render(glyph)
}

func (m *Model) renderFilterInput() string {
	inputStyle := lipgloss.NewStyle().
		Foreground(m.colors.base).
		Background(m.colors.info).
		Padding(0, 1)
	return inputStyle.Render("/ " + m.filterInput.View())
}

func (m *Model) renderActiveFilter() string {
	filterStyle := lipgloss.NewStyle().
		Foreground(m.colors.base).
		Background(m.colors.warning).
		Bold(true).
		Padding(0, 1)

	clearStyle := lipgloss.NewStyle().
		Foreground(m.colors.base).
		Background(m.colors.err).
		Padding(0, 1)

	filterText := m.filterQuery
	if len(m.filterProjectIDs) > 0 && m.filterQuery == "" {
		count := len(m.filterProjectIDs)
		if count == 1 {
			for id := range m.filterProjectIDs {
				if p := m.globalStore.GetProject(id); p != nil {
					filterText = "@" + p.Name
				}
				break
			}
		} else {
			filterText = fmt.Sprintf("%d projects", count)
		}
	}

	return filterStyle.Render("FILTERED: "+filterText) + " " + clearStyle.Render("× clear")
}

func (m *Model) renderFilterHint() string {
	return lipgloss.NewStyle().
		Foreground(m.colors.muted).
		Render("/ search (@project to filter)")
}

func (m *Model) countVisibleTickets() int {
	count := 0
	for _, tickets := range m.columnTickets {
		count += len(tickets)
	}
	return count
}

func (m *Model) renderProjectSelector() string {
	projects := m.globalStore.Projects()
	if len(projects) == 0 {
		return m.dimStyle().Render("No projects yet — press Enter to add one")
	}

	if m.ticketFormField != formFieldProject {
		if m.selectedProject != nil {
			return lipgloss.NewStyle().Foreground(m.colors.info).Render(m.selectedProject.Name)
		}
		return m.dimStyle().Render("Tab to select project")
	}

	if m.showAddProjectForm {
		return m.renderAddProjectForm()
	}

	var lines []string
	for i, p := range projects {
		name := p.Name
		path := shortenPath(p.RepoPath)

		nameStyle := lipgloss.NewStyle().Foreground(m.colors.text)
		pathStyle := lipgloss.NewStyle().Foreground(m.colors.muted)
		prefix := "  "

		var line string
		if i == m.projectListIndex {
			nameStyle = nameStyle.Foreground(m.colors.info).Bold(true)
			pathStyle = pathStyle.Foreground(m.colors.subtext)
			prefix = lipgloss.NewStyle().Foreground(m.colors.info).Render("● ")
			content := prefix + nameStyle.Render(name) + "  " + pathStyle.Render(path)
			line = lipgloss.NewStyle().Background(m.colors.surface).Padding(0, 1).Render(content)
		} else {
			prefix = "○ "
			line = prefix + nameStyle.Render(name) + "  " + pathStyle.Render(path)
		}
		lines = append(lines, line)
	}

	addOption := "○ " + lipgloss.NewStyle().Foreground(m.colors.success).Render("+ Add project...")
	if m.projectListIndex == len(projects) {
		content := lipgloss.NewStyle().Foreground(m.colors.info).Render("● ") +
			lipgloss.NewStyle().Foreground(m.colors.success).Bold(true).Render("+ Add project...")
		addOption = lipgloss.NewStyle().Background(m.colors.surface).Padding(0, 1).Render(content)
	}
	lines = append(lines, addOption)
	lines = append(lines, "")
	lines = append(lines, m.dimStyle().Render("↑↓ navigate  ⏎ select  d delete"))

	return strings.Join(lines, "\n")
}

func (m *Model) renderAddProjectForm() string {
	titleStyle := lipgloss.NewStyle().Foreground(m.colors.success).Bold(true)
	descStyle := lipgloss.NewStyle().Foreground(m.colors.muted).Italic(true)
	return titleStyle.Render("Add Project") + "\n\n" +
		"  " + lipgloss.NewStyle().Foreground(m.colors.subtext).Render("Repository path:") + "\n" +
		"  " + descStyle.Render("Path to a git repository (e.g. ~/projects/myapp)") + "\n" +
		"  " + m.addProjectPath.View() + "\n\n" +
		"  " + m.dimStyle().Render("⏎ Add  Esc Cancel")
}

func (m *Model) renderCreateProjectForm() string {
	titleStyle := lipgloss.NewStyle().
		Foreground(m.colors.success).
		Bold(true)

	labelStyle := lipgloss.NewStyle().Foreground(m.colors.info).Bold(true)
	descStyle := lipgloss.NewStyle().Foreground(m.colors.muted).Italic(true)

	var errorLine string
	if m.notification != "" {
		errorStyle := lipgloss.NewStyle().Foreground(m.colors.err).Bold(true)
		errorLine = "\n  " + errorStyle.Render("⚠ "+m.notification) + "\n"
	}

	content := titleStyle.Render("◈ Add Project") + "\n\n" +
		"  " + labelStyle.Render("Repository Path") + "\n" +
		"  " + descStyle.Render("Absolute path to a git repository") + "\n" +
		"  " + m.addProjectPath.View() + errorLine + "\n" +
		"  " + descStyle.Render("The project name will be derived from the directory name.") + "\n" +
		"  " + descStyle.Render("Example: ~/projects/myapp → \"myapp\"") + "\n\n" +
		"  " + lipgloss.NewStyle().Foreground(m.colors.success).Render("[Enter]") + m.dimStyle().Render(" Add  ") +
		lipgloss.NewStyle().Foreground(m.colors.muted).Render("[Esc]") + m.dimStyle().Render(" Cancel")

	formWidth := min(55, m.width-4)
	if formWidth < 40 {
		formWidth = 40
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.colors.success).
		Padding(1, 2).
		Width(formWidth).
		Render(content)
}

func shortenPath(path string) string {
	home, _ := os.UserHomeDir()
	if strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

func (m *Model) renderSidebar() string {
	if !m.sidebarVisible {
		return ""
	}

	projects := m.globalStore.Projects()
	// Match the board area exactly so JoinHorizontal(Top, sidebar, board)
	// produces a row that fits within m.height. Previously this used
	// m.height - headerHeight() - 1 (only the statusBar), which left the
	// sidebar 2 rows taller than boardAreaHeight() and pushed View()
	// output past m.height. See model.go:boardAreaHeight for the
	// canonical formula.
	availableHeight := m.boardAreaHeight()

	titleStyle := lipgloss.NewStyle().
		Foreground(m.colors.primary).
		Bold(true)

	selectedStyle := lipgloss.NewStyle().
		Foreground(m.colors.base).
		Background(m.colors.primary).
		Bold(true).
		Padding(0, 1)

	normalStyle := lipgloss.NewStyle().
		Foreground(m.colors.text).
		Padding(0, 1)

	checkStyle := lipgloss.NewStyle().Foreground(m.colors.success).Bold(true)
	uncheckStyle := lipgloss.NewStyle().Foreground(m.colors.muted)

	var lines []string

	lines = append(lines, titleStyle.Render("  Projects"))
	lines = append(lines, "")

	allCount := m.globalStore.Count()
	selectedCount := len(m.filterProjectIDs)
	noFilter := selectedCount == 0
	var allLabel string
	if noFilter {
		allLabel = fmt.Sprintf("[✓] All (%d)", allCount)
	} else if selectedCount == len(projects) {
		allLabel = fmt.Sprintf("[✓] All (%d)", allCount)
	} else {
		allLabel = fmt.Sprintf("[-] %d/%d", selectedCount, len(projects))
	}

	if m.sidebarIndex == 0 && m.sidebarFocused {
		lines = append(lines, selectedStyle.Render(allLabel))
	} else if noFilter || selectedCount == len(projects) {
		lines = append(lines, checkStyle.Render(allLabel))
	} else {
		lines = append(lines, normalStyle.Render(allLabel))
	}

	lines = append(lines, "")

	for i, p := range projects {
		idx := i + 1
		count := 0
		for _, t := range m.globalStore.All() {
			if t.ProjectID == p.ID {
				count++
			}
		}

		isSelected := m.filterProjectIDs[p.ID]
		var checkbox string
		if noFilter {
			checkbox = "    "
		} else if isSelected {
			checkbox = "[✓] "
		} else {
			checkbox = "[ ] "
		}
		label := fmt.Sprintf("%s%s (%d)", checkbox, p.Name, count)

		if m.sidebarIndex == idx && m.sidebarFocused {
			lines = append(lines, selectedStyle.Render(label))
		} else if isSelected {
			lines = append(lines, checkStyle.Render(label))
		} else {
			lines = append(lines, uncheckStyle.Render(label))
		}
	}

	lines = append(lines, "")
	addIndex := len(projects) + 1
	if m.sidebarIndex == addIndex && m.sidebarFocused {
		lines = append(lines, selectedStyle.Render("+ Add project"))
	} else {
		addStyle := lipgloss.NewStyle().Foreground(m.colors.success).Padding(0, 1)
		lines = append(lines, addStyle.Render("+ Add project"))
	}

	for len(lines) < availableHeight-2 {
		lines = append(lines, "")
	}

	hintStyle := lipgloss.NewStyle().Foreground(m.colors.muted).Italic(true)
	if m.sidebarFocused {
		lines = append(lines, hintStyle.Render("  j/k ⏎toggle a/d"))
	} else {
		lines = append(lines, hintStyle.Render("  h→focus  [hide"))
	}

	content := strings.Join(lines, "\n")

	style := lipgloss.NewStyle().
		Width(m.sidebarWidth).
		Height(availableHeight).
		BorderRight(true).
		BorderStyle(lipgloss.NormalBorder())

	if m.sidebarFocused {
		style = style.BorderForeground(m.colors.primary)
	} else {
		style = style.BorderForeground(m.colors.surface)
	}

	return style.Render(content)
}

func (m *Model) boardWidth() int {
	if m.sidebarVisible {
		return m.width - m.sidebarWidth - 1
	}
	return m.width
}

type uiColors struct {
	base      lipgloss.Color
	surface   lipgloss.Color
	overlay   lipgloss.Color
	text      lipgloss.Color
	subtext   lipgloss.Color
	muted     lipgloss.Color
	primary   lipgloss.Color
	secondary lipgloss.Color
	success   lipgloss.Color
	warning   lipgloss.Color
	err       lipgloss.Color
	info      lipgloss.Color
}

func newUIColors(theme config.Theme) uiColors {
	return uiColors{
		base:      lipgloss.Color(theme.Colors.Base),
		surface:   lipgloss.Color(theme.Colors.Surface),
		overlay:   lipgloss.Color(theme.Colors.Overlay),
		text:      lipgloss.Color(theme.Colors.Text),
		subtext:   lipgloss.Color(theme.Colors.Subtext),
		muted:     lipgloss.Color(theme.Colors.Muted),
		primary:   lipgloss.Color(theme.Colors.Primary),
		secondary: lipgloss.Color(theme.Colors.Secondary),
		success:   lipgloss.Color(theme.Colors.Success),
		warning:   lipgloss.Color(theme.Colors.Warning),
		err:       lipgloss.Color(theme.Colors.Error),
		info:      lipgloss.Color(theme.Colors.Info),
	}
}

var (
	columnBorder = lipgloss.Border{
		Top:         "━",
		Bottom:      "━",
		Left:        "┃",
		Right:       "┃",
		TopLeft:     "┏",
		TopRight:    "┓",
		BottomLeft:  "┗",
		BottomRight: "┛",
	}

	columnBorderActive = lipgloss.Border{
		Top:         "━",
		Bottom:      "━",
		Left:        "┃",
		Right:       "┃",
		TopLeft:     "┏",
		TopRight:    "┓",
		BottomLeft:  "┗",
		BottomRight: "┛",
	}

	dragTargetBorder = lipgloss.Border{
		Top:         "═",
		Bottom:      "═",
		Left:        "║",
		Right:       "║",
		TopLeft:     "╔",
		TopRight:    "╗",
		BottomLeft:  "╚",
		BottomRight: "╝",
	}

	ticketBorder = lipgloss.Border{
		Top:         "─",
		Bottom:      "─",
		Left:        "│",
		Right:       "│",
		TopLeft:     "╭",
		TopRight:    "╮",
		BottomLeft:  "╰",
		BottomRight: "╯",
	}

	ticketBorderSelected = lipgloss.Border{
		Top:         "═",
		Bottom:      "═",
		Left:        "║",
		Right:       "║",
		TopLeft:     "╔",
		TopRight:    "╗",
		BottomLeft:  "╚",
		BottomRight: "╝",
	}
)

func (m *Model) dimStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(m.colors.muted)
}

func (m *Model) columnColor(status board.TicketStatus) lipgloss.Color {
	switch status {
	case board.StatusBacklog:
		return m.colors.primary
	case board.StatusInProgress:
		return m.colors.warning
	case board.StatusInReview:
		return m.colors.secondary
	case board.StatusDone:
		return m.colors.success
	default:
		return m.colors.muted
	}
}

func (m *Model) renderThemeDropdown() string {
	themes := config.ThemeNames()
	if len(themes) == 0 {
		return m.dimStyle().Render("    No themes available")
	}

	var lines []string
	lines = append(lines, "")

	maxVisible := 8
	startIdx := 0
	if m.themeListIndex >= maxVisible {
		startIdx = m.themeListIndex - maxVisible + 1
	}
	endIdx := startIdx + maxVisible
	if endIdx > len(themes) {
		endIdx = len(themes)
	}

	if startIdx > 0 {
		lines = append(lines, m.dimStyle().Render(fmt.Sprintf("      ▲ %d more", startIdx)))
	}

	for i := startIdx; i < endIdx; i++ {
		theme := themes[i]
		isSelected := i == m.themeListIndex

		style := lipgloss.NewStyle().Foreground(m.colors.subtext)
		prefix := "      ○ "

		if isSelected {
			style = lipgloss.NewStyle().Foreground(m.colors.info).Bold(true)
			prefix = "      ● "
		}

		lines = append(lines, prefix+style.Render(theme))
	}

	if endIdx < len(themes) {
		remaining := len(themes) - endIdx
		lines = append(lines, m.dimStyle().Render(fmt.Sprintf("      ▼ %d more", remaining)))
	}

	lines = append(lines, "")
	lines = append(lines, m.dimStyle().Render("      ↑↓ navigate  Enter select  Esc cancel"))

	return strings.Join(lines, "\n")
}
