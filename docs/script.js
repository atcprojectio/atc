document.addEventListener('DOMContentLoaded', () => {
  const sidebarLinks = document.querySelectorAll('.sidebar-nav a[href^="#"]');
  const sections = document.querySelectorAll('.content-section');
  const searchInput = document.getElementById('docs-search');
  const mobileMenuBtn = document.getElementById('mobile-menu-btn');
  const sidebar = document.getElementById('sidebar');
  const tocList = document.getElementById('toc-list');

  // 1. Mobile Menu Toggle
  if (mobileMenuBtn && sidebar) {
    mobileMenuBtn.addEventListener('click', () => {
      sidebar.classList.toggle('menu-open');
      const icon = mobileMenuBtn.querySelector('i');
      if (sidebar.classList.contains('menu-open')) {
        icon.className = 'fa-solid fa-xmark';
      } else {
        icon.className = 'fa-solid fa-bars';
      }
    });
  }

  // Close sidebar when clicking menu items on mobile
  document.querySelectorAll('.sidebar-nav a').forEach(link => {
    link.addEventListener('click', () => {
      if (sidebar && sidebar.classList.contains('menu-open')) {
        sidebar.classList.remove('menu-open');
        if (mobileMenuBtn) {
          mobileMenuBtn.querySelector('i').className = 'fa-solid fa-bars';
        }
      }
    });
  });

  // 2. Navigation & Tab/Section Switching
  function showSection(targetId) {
    const cleanId = targetId.replace('#', '');
    
    // Hide all sections, show target section
    sections.forEach(sec => {
      if (sec.id === cleanId) {
        sec.classList.add('active-section');
      } else {
        sec.classList.remove('active-section');
      }
    });

    // Update active link state in sidebar
    sidebarLinks.forEach(link => {
      if (link.getAttribute('href') === `#${cleanId}`) {
        link.classList.add('active');
      } else {
        link.classList.remove('active');
      }
    });

    // Generate Table of Contents (TOC) for the active section
    generateTOC(cleanId);
    
    // Scroll window back to top of documentation content area
    window.scrollTo({ top: 0, behavior: 'instant' });
  }

  // Listen to hash changes or clicks on nav links
  sidebarLinks.forEach(link => {
    link.addEventListener('click', (e) => {
      e.preventDefault();
      const href = link.getAttribute('href');
      window.location.hash = href;
      showSection(href);
    });
  });

  // Handle initial load with hash
  if (window.location.hash) {
    showSection(window.location.hash);
  } else {
    // Default to first active section
    const activeSection = document.querySelector('.content-section.active-section');
    if (activeSection) {
      generateTOC(activeSection.id);
    }
  }

  // 3. Generate Table of Contents (TOC) for current section
  function generateTOC(sectionId) {
    if (!tocList) return;
    tocList.innerHTML = '';
    
    const targetSection = document.getElementById(sectionId);
    if (!targetSection) return;

    // Find all H2 headings in the current section
    const headings = targetSection.querySelectorAll('h2');
    
    if (headings.length === 0) {
      const emptyLi = document.createElement('li');
      emptyLi.textContent = 'No sub-sections';
      emptyLi.style.color = 'var(--text-muted)';
      tocList.appendChild(emptyLi);
      return;
    }

    headings.forEach((heading, idx) => {
      // Ensure heading has an ID
      if (!heading.id) {
        heading.id = `${sectionId}-sub-${idx}`;
      }

      const li = document.createElement('li');
      const a = document.createElement('a');
      a.href = `#${heading.id}`;
      a.textContent = heading.textContent;
      
      a.addEventListener('click', (e) => {
        e.preventDefault();
        heading.scrollIntoView({ behavior: 'smooth' });
        
        // Highlight active TOC item
        tocList.querySelectorAll('a').forEach(item => item.classList.remove('active'));
        a.classList.add('active');
      });

      li.appendChild(a);
      tocList.appendChild(li);
    });
  }

  // 4. Interactive Search
  if (searchInput) {
    searchInput.addEventListener('input', (e) => {
      const val = e.target.value.toLowerCase().trim();
      if (val === '') {
        // Restore active section normally
        const currentHash = window.location.hash || '#overview';
        showSection(currentHash);
        return;
      }

      // Filter sections and highlight matching text elements
      sections.forEach(sec => {
        const text = sec.textContent.toLowerCase();
        if (text.includes(val)) {
          sec.classList.add('active-section');
        } else {
          sec.classList.remove('active-section');
        }
      });
    });
  }

  // 5. Setup Copy to Clipboard for code blocks
  document.querySelectorAll('pre').forEach(preBlock => {
    // Create copy button
    const btn = document.createElement('button');
    btn.className = 'btn-copy';
    btn.innerHTML = '<i class="fa-regular fa-copy"></i>';
    btn.title = 'Copy to clipboard';
    
    btn.addEventListener('click', async () => {
      const codeBlock = preBlock.querySelector('code');
      const text = codeBlock ? codeBlock.textContent : preBlock.textContent;
      
      try {
        await navigator.clipboard.writeText(text);
        btn.classList.add('success');
        btn.innerHTML = '<i class="fa-solid fa-check"></i>';
        
        setTimeout(() => {
          btn.classList.remove('success');
          btn.innerHTML = '<i class="fa-regular fa-copy"></i>';
        }, 2000);
      } catch (err) {
        console.error('Failed to copy to clipboard', err);
      }
    });

    preBlock.appendChild(btn);
  });
});
